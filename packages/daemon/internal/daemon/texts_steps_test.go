package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/sjawhar/legion/daemon/internal/texts"
)

// goldenStep is one step this change took out of a daemon text, for one set of its readers: the
// kind, main's sentence as the Go literal of its builder at b4a566cb, or at 237f31c7, 724196ef,
// c7e1cbd5, 3d2dddd0, 6d50ef31, a4cd4949 or 5f2bf9af for a sentence main wrote since (a format
// verb, %s, %[1]s or %d, and … stand for any text), the readers, and the passage of the readers'
// prompt (packages/daemon/internal/prompts) that gives each of them its own rule for the step,
// quoted word for word from one line of the file.
type goldenStep struct {
	kind     texts.Kind
	sentence string
	readers  string
	file     string
	passage  string
}

const (
	goPrompts   = "packages/daemon/internal/prompts/go/"
	rolePrompts = "packages/daemon/internal/prompts/roles/"
)

// goldenSteps is the table of every step taken out of a daemon text, for each of its readers.
var goldenSteps = []goldenStep{
	{texts.PhaseAssignment, " Handoff: %s version %d (dispatch handoff-read --issue %s --phase %s --version %d).", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "Your task names the predecessor's handoff in two fields, `handoff_phase` (its `<word>`) and `handoff_version` (its `<n>`): read it with `dispatch handoff-read --issue <issue> --phase <word> --version <n>`."},
	{texts.PhaseAssignment, "Continue %s. Issue: %s. Phase: %s.", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "A `phase-assignment` task names the issue's `title` and the `phase` you start: do that phase's work."},
	{texts.PhaseAssignment, " Head %s is the merge of %s forward that landed after the review, and the reviewer is asked to read it: merge head %s only once the reviewer's approval of it is on the pull request.", "merger",
		goPrompts + "merger.md", "that head is the merge of the base `merged_forward` names, which landed after the review, and the reviewer is asked to read it, so merge that head only once the reviewer's approval of it is on the pull request."},
	{texts.PhaseAssignment, "the reviewer completed its round on pull request #%d with no review that decides it: only an APPROVE of head %s or a REQUEST_CHANGES ends the round", "reviewer",
		goPrompts + "reviewer.md", "`stuck_round: undecided` means that no review decided the round."},
	{texts.PhaseAssignment, "; a review that would otherwise have decided this round was set aside - ", "reviewer",
		goPrompts + "reviewer.md", "`set_aside` says why the daemon set aside a review that would otherwise have decided the round."},
	{texts.PhaseAssignment, "the reviewer approved %s on pull request #%d, which does not approve head %s: a push since may have changed code, so only an APPROVE of %s or a REQUEST_CHANGES ends the round", "reviewer",
		goPrompts + "reviewer.md", "`stuck_round: approved-other-code` means that you approved a head whose code the current head may not carry, and `reviewed_head` names that approved head."},
	{texts.PhaseAssignment, "the reviewer approved head %s on pull request #%d, and GitHub reports that head %s rather than one it would merge.", "reviewer",
		goPrompts + "reviewer.md", "`stuck_round: merge-state` means that GitHub will not merge the head you approved, as an `approved-round-stuck` message says."},
	{texts.PhaseAssignment, "; a skipped check ran nothing on this head", "reviewer",
		goPrompts + "reviewer.md", "Its `each_skipped_check: ran_nothing` says once that a skipped check ran nothing on this head, so it is no pass."},
	{texts.PhaseResume, " Resume the existing phase work.", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "A `phase-resume` task names a phase that is already under way: resume the work of that phase where it stands."},
	{texts.CheckLook, "unknown, read it from the pull request on GitHub", "tester",
		goPrompts + "tester.md", "Where the task names no merge commit, read it from the pull request on GitHub."},
	{texts.CheckLook, "unknown, read it from the pull request on GitHub", "implementer",
		goPrompts + "implementer.md", "Where the task names no merge commit, read it from the pull request on GitHub."},
	{texts.CheckLook, " This is the last look: no look follows.", "tester",
		goPrompts + "tester.md", "The task of the bound's look carries `last: true`"},
	{texts.CheckLook, " This is the last look: no look follows.", "implementer",
		goPrompts + "implementer.md", "whether it is the last (`last: true`)"},
	{texts.CheckLook, "Do what the deployment instructions say to reach production and read whether production carries the merge. ", "tester",
		goPrompts + "tester.md", "Do what the deployment instructions say to reach production, read whether production carries the merge, then report with `handoff_complete` and a `verdict`, committing nothing and writing no handoff"},
	{texts.CheckLook, "Do what the deployment instructions say to reach production and read whether production carries the merge. ", "implementer",
		goPrompts + "implementer.md", "Each look reaches production as the deployment instructions say and reads whether production carries the merge."},
	{texts.CheckLook, "Where it does, drive the changed path and observe each requirement of the issue that only the deploy can show, read from the spec version that bound the review, each as the requirement names it: observing something else is never that requirement observed. ", "tester",
		rolePrompts + "tester.md", "`pass` covers the changed path and each requirement of the issue that only the deploy can show, read from the spec version that bound the review, and `observed` names each and what was seen."},
	{texts.CheckLook, "Where it does, drive the changed path and observe each requirement of the issue that only the deploy can show, read from the spec version that bound the review, each as the requirement names it: observing something else is never that requirement observed. ", "implementer",
		rolePrompts + "implementer.md", "`pass` covers the changed path and each requirement of the issue that only the deploy can show, read from the spec version that bound the review, and `observed` names each and what was seen."},
	{texts.CheckLook, "pass only once you drove the changed path in production and observed every one of those requirements, and recorded the issue's after_merge check with dispatch result at the merge commit, its observed naming each requirement and what you saw of it and what woke this look, and put the line that command prints in the pull request body; ", "tester",
		goPrompts + "tester.md", "`pass` once you drove the changed path in production and recorded the result against the issue's `after_merge` check with `dispatch result` at the merge commit, naming what woke this look, with the line it prints in the pull request body's `Checks` block"},
	{texts.CheckLook, "pass only once you drove the changed path in production and observed every one of those requirements, and recorded the issue's after_merge check with dispatch result at the merge commit, its observed naming each requirement and what you saw of it and what woke this look, and put the line that command prints in the pull request body; ", "implementer",
		goPrompts + "implementer.md", "Put the line that command prints in the pull request body's `Checks` block."},
	{texts.CheckLook, " Report pending when production does not carry the merge yet, or carries it with one of those requirements still unobserved and a look still to follow, the summary naming what is missing and what you will look for next, with no result recorded and the pull request body saying the check is still open on the merge commit.", "tester",
		goPrompts + "tester.md", "nothing recorded and that line left at `- Check <block id> (after_merge): not yet run`, which says that the check is still open on the merge commit"},
	{texts.CheckLook, " Report pending when production does not carry the merge yet, or carries it with one of those requirements still unobserved and a look still to follow, the summary naming what is missing and what you will look for next, with no result recorded and the pull request body saying the check is still open on the merge commit.", "implementer",
		goPrompts + "implementer.md", "Record nothing for it, and leave the check's line in the pull request body at `- Check <block id> (after_merge): not yet run`, which says that the check is still open on the merge commit."},
	{texts.CheckLook, "fail for a defect, never an issue you file (skill://legion-worker/references/findings.md), …; record the after_merge check as a fail with dispatch result at the merge commit, and name the message or the ask in its observed, in the line you put in the pull request body and in the summary.", "tester",
		goPrompts + "tester.md", "never an issue you file (`skill://legion-worker/references/findings.md`)"},
	{texts.CheckLook, "record the after_merge check as a fail with dispatch result at the merge commit, and name the message or the ask in its observed, in the line you put in the pull request body and in the summary.", "tester",
		goPrompts + "tester.md", "The result's `observed`, the line you put in the pull request body and the summary name the message or the ask."},
	{texts.CheckLook, "fail for a defect, never an issue you file (skill://legion-worker/references/findings.md), …; record the after_merge check as a fail with dispatch result at the merge commit, and name the message or the ask in its observed, in the line you put in the pull request body and in the summary.", "implementer",
		goPrompts + "implementer.md", "Report `fail` for a defect, never an issue you file (`skill://legion-worker/references/findings.md`)."},
	{texts.CheckLook, "record the after_merge check as a fail with dispatch result at the merge commit, and name the message or the ask in its observed, in the line you put in the pull request body and in the summary.", "implementer",
		goPrompts + "implementer.md", "Record the `after_merge` check as a fail with `dispatch result` at the merge commit, and name the message or the ask in its `observed`, in the line you put in the pull request body and in the summary."},
	{texts.CheckLook, "and a requirement observed as not met, or still unobserved when no look follows this one, whether or not production carries the merge, is a defect", "tester",
		rolePrompts + "core/tester.md", "One observed as not met, or still unobserved when the check reaches its last look, is a failed check whether or not production carries the merge, never a check closed as not run"},
	{texts.CheckLook, "and a requirement observed as not met, or still unobserved when no look follows this one, whether or not production carries the merge, is a defect", "implementer",
		goPrompts + "implementer.md", "one observed as not met is a `fail`, and one still unobserved at the last look is a `fail` too, whether or not production carries the merge"},
	{texts.CheckLook, " Report pending only for an issue with none of those requirements, the summary naming what you could not drive, with no result recorded.", "tester",
		goPrompts + "tester.md", "and that look's `pending`, only for an issue with no requirement that only the deploy can show, carries no `wake_at`"},
	{texts.CheckLook, " Report pending only for an issue with none of those requirements, the summary naming what you could not drive, with no result recorded.", "tester",
		goPrompts + "tester.md", "The summary of that `pending` names what you could not drive, with no result recorded."},
	{texts.CheckLook, " Report pending only for an issue with none of those requirements, the summary naming what you could not drive, with no result recorded.", "implementer",
		goPrompts + "implementer.md", "On the check's own last look, report `pending` only for an issue with no requirement that only the deploy can show, its summary naming what you could not drive, with no result recorded."},
	{texts.CheckLook, "and a requirement observed as not met, or still unobserved when no look follows this one, whether or not production carries the merge, is a defect", "tester",
		goPrompts + "tester.md", "`unobserved_requirement: fail` says that a requirement only the deploy can show that is still unobserved at the last look is a `fail` whether or not production carries the merge"},
	{texts.CheckLook, "and a requirement observed as not met, or still unobserved when no look follows this one, whether or not production carries the merge, is a defect", "implementer",
		goPrompts + "implementer.md", "`unobserved_requirement: fail` says that a requirement only the deploy can show that is still unobserved at the last look is a `fail` whether or not production carries the merge"},
	{texts.CheckLook, "and a requirement observed as not met, or still unobserved when no look follows this one, whether or not production carries the merge, is a defect", "tester",
		goPrompts + "tester.md", "`not_met_requirement: fail` says that a requirement only the deploy can show that the look observes as not met is a `fail`, on every look, the last among them."},
	{texts.CheckLook, "and a requirement observed as not met, or still unobserved when no look follows this one, whether or not production carries the merge, is a defect", "tester",
		rolePrompts + "tester.md", "`not_met_requirement: fail` says that a requirement only the deploy can show that the look observes as not met is a `fail`, on every look, the last among them."},
	{texts.CheckLook, "and a requirement observed as not met, or still unobserved when no look follows this one, whether or not production carries the merge, is a defect", "implementer",
		goPrompts + "implementer.md", "`not_met_requirement: fail` says that a requirement only the deploy can show that the look observes as not met is a `fail`, on every look, the last among them."},
	{texts.CheckLook, " Report pending only for an issue with none of those requirements, the summary naming what you could not drive, with no result recorded.", "tester",
		goPrompts + "tester.md", "`pending: only_with_no_deploy_only_requirement` says that the last look's `pending` is only for an issue with no such requirement."},
	{texts.CheckLook, " Report pending only for an issue with none of those requirements, the summary naming what you could not drive, with no result recorded.", "implementer",
		goPrompts + "implementer.md", "`pending: only_with_no_deploy_only_requirement` says that the last look's `pending` is only for an issue with no such requirement."},
	{texts.CheckLook, " This last look's pending carries no wake_at and closes the check as not run: before reporting it, write on the pull request why the check did not run within the bound.", "tester",
		goPrompts + "tester.md", "so before reporting it write on the pull request why the check did not run within the bound."},
	{texts.CheckLook, " This last look's pending carries no wake_at and closes the check as not run: before reporting it, write on the pull request why the check did not run within the bound.", "implementer",
		goPrompts + "implementer.md", "That last look's `pending` carries no `wake_at` and closes the check as not run: before you report it, write on the pull request why the check did not run within the bound."},
	{texts.CheckLook, "At that report the daemon stops this process and keeps this session, and your next look resumes it with a task naming the summary you gave as what woke it.", "tester",
		goPrompts + "tester.md", "the next look brings the pod back and resumes this session with a new task that names the summary you sent as what woke it."},
	{texts.CheckLook, " Wait for nothing in this session: end the turn with the report.", "tester",
		goPrompts + "tester.md", "So wait for nothing in this session: end the turn with the report."},
	{texts.CheckLook, " Wait for nothing in this session: end the turn with the report.", "implementer",
		goPrompts + "implementer.md", "Wait for nothing in the session: end the turn with the report."},
	{texts.RootResume, "Your ask %s on %s was retracted when its decision block left the spec, so no person will answer it. Read the spec as it stands; if you still need the decision, ask again.", "architect (root)",
		rolePrompts + "architect-root.md", "After a retraction (`act: retracted`), no person will answer that ask, because its decision block left the spec: read the spec as it stands, and ask again only if you still need the decision."},
	{texts.RootResume, "Your ask %s on %s was closed by %s, so no person will answer it. Read its thread; if you still need the decision, ask again.", "architect (root)",
		rolePrompts + "architect-root.md", "After a close by anyone who is not a person (`act: closed`, where `closed_by` names who closed it), no person will answer that ask either: read its thread, and ask again only if you still need the decision."},
	{texts.RootResume, " Waiting on you: %s; read each thread and reply.", "architect (root)",
		rolePrompts + "architect-root.md", "Each ask that the task lists as waiting on you (`waiting_on_you`) is yours to answer: read its thread and reply in it."},
	{texts.RootResume, " Still with the person: %s. Hand each one back once you have done what the answer asks — a reply in its thread that leaves the turn with the person", "architect (root)",
		rolePrompts + "architect-root.md", "Hand an item back with a reply in its thread that leaves the turn with the person, or, while the gate is armed and closed, with a new approval request and `register_gate` of that version."},
	{texts.RootResume, " — or close it if you no longer need it. Handing one back starts your next wait; then end your turn.", "architect (root)",
		rolePrompts + "architect-root.md", "Close an item that you no longer need instead of handing it back."},
	{texts.RootResume, " — or close it if you no longer need it. Handing one back starts your next wait; then end your turn.", "architect (root)",
		rolePrompts + "architect-root.md", "Handing an item back starts your next wait, so end your turn after it."},
	{texts.RootResume, " When the %s is ready, hand the request back to the person: request approval again and register that version with register_gate", "architect (root)",
		rolePrompts + "architect-root.md", "When nothing is still with the person and the gate is armed and closed, hand the request back once the document is ready: request approval again and register that version with `register_gate`, or answer in the request's thread while that thread is open (`request_open`)."},
	{texts.RootResume, " The design gate is closed at version %d, as no registration of that version has opened it: when the %s is ready, register it at its current version, which opens the gate with no approval.", "architect (root)",
		rolePrompts + "architect-root.md", "Where the gate is not armed (`gate_armed: false`), no person approves the document: when it is ready, register it at its current version, which opens the gate with no approval."},
	{texts.RootResume, " Read the review, and revise the %s as they ask.", "architect (root)",
		rolePrompts + "architect-root.md", "A changes request asks you to revise the document as the person asks in its reason, except where the gate is open and armed: a person then approved the document after that review, so it asks for no revision now."},
	{texts.RootResume, " A person approved the %s after that review, so it asks for no revision now; read it.", "architect (root)",
		rolePrompts + "architect-root.md", "A changes request asks you to revise the document as the person asks in its reason, except where the gate is open and armed: a person then approved the document after that review, so it asks for no revision now."},
	{texts.RootResume, " A person then requested changes to your %s at %s. Read the review, and revise the %s as they ask.", "architect (root)",
		rolePrompts + "architect-root.md", "A changes request made after the wait began that did not answer it (`later_changes_version`, with its reason in the `later_reason` block) asks the same revision."},
	{texts.RootResume, "A newer version of your %s on %s, %s, handed the approval request back to you. Read it and the request's thread.", "architect (root)",
		rolePrompts + "architect-root.md", "A newer version that handed the approval request back to you (`act: version`) asks you to read that version and the request's thread."},
	{texts.RootResume, " Continue.", "architect (root)",
		rolePrompts + "architect-root.md", "Where nothing is with the person, nothing waits on you and the gate is open, continue the tree's work."},
	{texts.RootResume, "; read it.", "architect (root)",
		rolePrompts + "architect-root.md", "carries the person's words in a block, so read those words in the task."},
	{texts.RootResume, "; read that thread.", "architect (root)",
		rolePrompts + "architect-root.md", "carries the person's words in a block, so read those words in the task."},
	{texts.RootResume, "; read its thread.", "architect (root)",
		rolePrompts + "architect-root.md", "carries the person's words in a block, so read those words in the task."},
	{texts.RootResume, "; read the issue's asks and its spec.", "architect (root)",
		rolePrompts + "architect-root.md", "carries the person's words in a block, so read those words in the task."},
	{texts.KeptDecisions, "While nobody in %s's tree could hear it, a person decided on the tree: %s. Read each before you continue.", "architect (root)",
		goPrompts + "architect-root.md", "Act on each one before you carry on."},
	{texts.KeptDecisions, "The %s is approved at version %d and is now at version %d: request approval of version %d, as skill://legion-architect/references/design-gate.md (\"The armed gate\") says.", "architect (root)",
		goPrompts + "architect-root.md", "request approval of that latest version, as `skill://legion-architect/references/design-gate.md` (\"The armed gate\") says."},
	{texts.ArchitectStart, "Take it up in this turn, as your role says: claim it in Dispatch", "architect (root)",
		goPrompts + "architect-root.md", "Take it up in this turn."},
	{texts.ArchitectStart, " Nothing else starts this tree, and no message is coming.", "architect (root)",
		goPrompts + "architect-root.md", "`nothing_else_starts_it: true` means that nothing else starts this tree and no message is coming."},
	{texts.ArchitectStart, "Decompose nothing and release no child.", "architect (root)",
		goPrompts + "architect-root.md", "`decompose: false` means that you decompose nothing and release no child."},
	{texts.ArchitectStart, " Its pull request has merged, so the daemon starts no worker for that phase, and the root stands there until the rest of its tree has left the workflow, when the daemon closes it.", "architect (root)",
		goPrompts + "architect-root.md", "which stands in its phase until the rest of its tree has left the workflow and the daemon closes it."},
	{texts.PodWake, "Your issue's pod had been released while it was idle, and this wake brought it back. Your session, your workspace and any work you had not pushed are as you left them. ", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "Your session, your workspace and the work you had not pushed are as you left them."},
	{texts.PodWake, "Your issue's pod had been released while it was idle, and this wake brought it back. Your session, your workspace and any work you had not pushed are as you left them. ", "architect",
		goPrompts + "architect-common.md", "Your session and your workspace are as you left them."},
	{texts.PodWake, ". Read the checks before you decide anything.", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "read the checks before you decide anything."},
	{texts.PodWake, ". Read the checks before you decide anything.", "architect",
		goPrompts + "architect-common.md", "read the checks before you decide anything."},
	{texts.PodWake, "; skipped %s: a skipped check ran nothing on this head", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "It names each check GitHub skipped (`skipped`), and its `each_skipped_check: ran_nothing` says once that a skipped check ran nothing on this head, so it is no pass"},
	{texts.PodWake, "; skipped %s: a skipped check ran nothing on this head", "architect",
		goPrompts + "architect-common.md", "each check GitHub skipped (`skipped`, beside `each_skipped_check: ran_nothing`, which says once that a skipped check ran nothing on this head, so it is no pass)"},
	{texts.PodWake, "A message sent with keep: false is not kept, and its sender sends it again.", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "A message sent with `keep: false` is not kept, and its sender sends it again."},
	{texts.PodWake, "A message sent with keep: false is not kept, and its sender sends it again.", "architect",
		goPrompts + "architect-common.md", "A message sent with `keep: false` is not kept, and its sender sends it again."},
	{texts.PodWake, ") while your pod was gone. A message that Envoy or Dispatch stored for your session reaches you in this session. ", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "A message that Envoy or Dispatch stored for your session reaches you in this session."},
	{texts.PodWake, ") while your pod was gone. A message that Envoy or Dispatch stored for your session reaches you in this session. ", "architect",
		goPrompts + "architect-common.md", "A message that Envoy or Dispatch stored for your session reaches you in this session."},
	{texts.PodWake, "A direct Envoy message that Envoy refused reaches you when its sender sends it again.", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "A direct Envoy message that Envoy refused reaches you when its sender sends it again."},
	{texts.PodWake, "A direct Envoy message that Envoy refused reaches you when its sender sends it again.", "architect",
		goPrompts + "architect-common.md", "A direct Envoy message that Envoy refused reaches you when its sender sends it again."},
	{texts.PodWake, ". Nothing else asked for you, so take up your phase where you left it.", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "`wake: backstop` means that nothing else asked for you: take up your phase where you left it."},
	{texts.PodWake, "Your project was paused, and its pause stopped your session. This wake started it again. Your pod, your workspace and any work you had not pushed are as you left them. ", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "`pause` is a pause of your project, which kept your pod."},
	{texts.PodWake, "Your project was paused, and its pause stopped your session. This wake started it again. Your pod, your workspace and any work you had not pushed are as you left them. ", "architect",
		goPrompts + "architect-common.md", "`pause` is a pause of your project, which kept your pod."},
	{texts.PodWake, ") while your session was stopped. A message that Envoy or Dispatch stored for your session reaches you in this session. ", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "`wake: agent_message` is a message sent to your session (`session`) while your session was stopped."},
	{texts.PodWake, ") while your session was stopped. A message that Envoy or Dispatch stored for your session reaches you in this session. ", "architect",
		goPrompts + "architect-common.md", "`wake: agent_message` is a message sent to your session (`session`) while your session was stopped."},
	{texts.RelaunchNote, "Before you do any of that work again, check what it already did.", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "Before you do any of that work again, check what it already did in your workspace and on the issue's branch."},
	{texts.RelaunchNote, "Before you do any of that work again, check what it already did.", "architect",
		goPrompts + "architect-common.md", "Before you do any of that work again, check what it already did."},
	{texts.RelaunchNote, "Before you do any of that work again, check what it already did.", "controller",
		rolePrompts + "controller-root.md", "Before you do any of that work again, check what it already did, with `read_state` and in Dispatch."},
	{texts.RelaunchNote, "Then continue the task.", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "Before you do any of that work again, check what it already did in your workspace and on the issue's branch."},
	{texts.RelaunchNote, "No result of that work will come to you.", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "Your tool calls in flight, and the subagents and background jobs that stopped, give you no result, and your transcript does not say so."},
	{texts.RelaunchNote, "No result of that work will come to you.", "architect",
		goPrompts + "architect-common.md", "Your tool calls in flight, and the subagents and background jobs that stopped, give you no result, and your transcript does not say so."},
	{texts.RelaunchNote, "No result of that work will come to you.", "controller",
		rolePrompts + "controller-root.md", "Your tool calls in flight, and the subagents and background jobs that stopped, give you no result, and your transcript does not say so."},
	{texts.RelaunchNote, "Your transcript does not say so.", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "Your tool calls in flight, and the subagents and background jobs that stopped, give you no result, and your transcript does not say so."},
	{texts.RelaunchNote, "Your transcript does not say so.", "architect",
		goPrompts + "architect-common.md", "Your tool calls in flight, and the subagents and background jobs that stopped, give you no result, and your transcript does not say so."},
	{texts.RelaunchNote, "Your transcript does not say so.", "controller",
		rolePrompts + "controller-root.md", "Your tool calls in flight, and the subagents and background jobs that stopped, give you no result, and your transcript does not say so."},
	{texts.AbandonedSessionNote, "Before you repeat any of that work, %s, and tell the architect your Legion addressing line names what you found, unless that is you or it names none.", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "Before you repeat any of that work, read the issue, then the `branch` the note names and the handoff records on the issue (`dispatch handoff-read --issue <issue> --phase <word>`, for `plan`, `implement`, `test` and `review`)."},
	{texts.AbandonedSessionNote, "read the issue, then your branch %s and the handoff records its phases wrote on the issue, which `dispatch handoff-read --issue %s --phase <word>` reads (%s)", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "Before you repeat any of that work, read the issue, then the `branch` the note names and the handoff records on the issue (`dispatch handoff-read --issue <issue> --phase <word>`, for `plan`, `implement`, `test` and `review`)."},
	{texts.AbandonedSessionNote, ", and tell the architect your Legion addressing line names what you found, unless that is you or it names none.", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "Then tell the architect that your `Legion addressing` line names what you found."},
	{texts.AbandonedSessionNote, ", and tell the architect your Legion addressing line names what you found, unless that is you or it names none.", "architect",
		goPrompts + "architect-common.md", "Where your `Legion addressing` line names an architect above you, tell it what you found."},
	{texts.AbandonedSessionNote, "Before you repeat any of that work, %s, and tell the architect", "controller",
		rolePrompts + "controller-root.md", "Before you repeat any of that work, read the issue."},
	{texts.OutOfMemoryNote, "The work that filled the pod can be another role's, not yours. An issue pod runs no heavy build and no whole test suite, so read skill://legion-worker/references/tools.md before you run such work again, and send it to CI on the pull request or where your deployment's instructions say.", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "After an `out-of-memory-note`, remember that the work that filled the pod can be another role's: read `skill://legion-worker/references/tools.md` before you run heavy work again, and send such work to CI on the pull request or where your deployment's instructions say."},
	{texts.OutOfMemoryNote, "The work that filled the pod can be another role's, not yours.", "architect",
		goPrompts + "architect-common.md", "which can be another role's work."},
	{texts.ProviderErrorNote, "Your work did not cause it. ", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "A `provider-error-note` says that a provider error stopped your turn, and your work did not cause it."},
	{texts.ProviderErrorNote, "Read what that turn already did in your workspace and on the issue's branch. Then continue the task.", "architect",
		goPrompts + "architect-common.md", "A `provider-error-note` says that a provider error stopped your turn, and your work did not cause it."},
	{texts.ProviderErrorNote, "Read what that turn already did in your workspace and on the issue's branch. Then continue the task.", "controller",
		rolePrompts + "controller-root.md", "Before you do any of that work again, check what it already did, with `read_state` and in Dispatch."},
	{texts.ChecksRed, "the head conflicts with %s: GitHub runs no checks on it; merge %s forward", "architect",
		goPrompts + "architect-common.md", "the daemon asked the implementer to merge the base forward onto a head that conflicts with its base, and the issue stays in the phase the notice names"},
	{texts.ChecksRed, ": the implementer is asked to merge %s forward, and the issue stays in %s", "architect",
		goPrompts + "architect-common.md", "the daemon asked the implementer to merge the base forward onto a head that conflicts with its base, and the issue stays in the phase the notice names"},
	{texts.CatchUp, "; start or resume it as your role says", "architect (root)",
		goPrompts + "architect-root.md", "Start or resume your tree from it as your role says."},
	{texts.CatchUp, "; start or resume your issue as your role says", "architect (child)",
		goPrompts + "architect.md", "Start or resume your issue from it as your role says."},
	{texts.CatchUp, "; in accepting, waiting for your acceptance, which no worker gives: %s (read the evidence, then sign_off or request_backward_move)", "architect (root)",
		goPrompts + "architect-root.md", "so read each one's evidence, then `sign_off` a parent only on an approved head that carries the merges of every child its criteria need"},
	{texts.CatchUp, "; in accepting, waiting for your acceptance, which no worker gives: %s (read the evidence, then sign_off or request_backward_move)", "architect (child)",
		goPrompts + "architect.md", "No worker runs there and the notice that moved it there may never have reached you, so read each one's evidence."},
	{texts.PRMerged, "Park one with park_child %s, and say in a comment which of those it is", "architect (root)",
		goPrompts + "architect-root.md", "Park one with `park_child` only when a person asked for that move, or when the work is done, folded into another issue or no longer needed, and say in a comment which of those it is."},
	{texts.PRMerged, "Park one with park_child %s, and say in a comment which of those it is", "architect (child)",
		goPrompts + "architect.md", "Park one with `park_child` only when a person asked for that move, or when the work is done, folded into another issue or no longer needed, and say in a comment which of those it is."},
	{texts.PullRequestMissing, "Tell its implementer (envoy_publish to its role topic) to open the pull request from %s; the daemon records it and moves %s to testing once it is open. ", "architect",
		goPrompts + "architect-common.md", "Tell the implementer with `envoy_publish` to its role topic to open the pull request from that branch; the daemon records it and moves the issue to `testing` once it is open."},
	{texts.PullRequestMissing, "When the work should not go on, decide %s's phase as for a pull request closed without merging.", "architect",
		goPrompts + "architect-common.md", "When the work should not go on, decide the issue's phase as for a `pr-closed-unmerged` notice."},
	{texts.MessageRefused, " settling that is yours — ask a person with `dispatch ask` to set the status on the issue.", "architect",
		goPrompts + "architect-common.md", "ask a person with `dispatch ask` to set that status on the issue, saying what the record reads"},
	{texts.MessageRefused, "; post what a person still needs yourself.", "architect",
		goPrompts + "architect-common.md", "what a person still needs is yours to post on the issue with `dispatch comment`."},
	{texts.ChildStatus, "Its own architect starts with it and owns it while that architect holds its role: its decomposition, its children and its acceptance. You hear its child-status and child-closed notices", "architect",
		goPrompts + "architect-common.md", "The child's own architect starts with it and owns it while that architect holds its role: its decomposition, its children and its acceptance."},
	{texts.ChildStatus, "Take %s back with a backward move to run %s under it now, or leave %s where it is and it starts once %s has closed.", "architect",
		goPrompts + "architect-common.md", "take the ancestor back with `request_backward_move` to run it under the ancestor now, or leave it to start at the ancestor's close."},
	{texts.ChildStatus, "It runs under its own architect, and its work reaches the tree of %s only through that tree's own issues.", "architect",
		goPrompts + "architect-common.md", "It runs under its own architect, and its work reaches your tree only through your tree's own issues."},
	{texts.ChildrenFirst, "Move a child to icebox %s, and cite that reason in a comment on the child. ", "architect",
		goPrompts + "architect-common.md", "move a child to `icebox` only when a person asked for that move, or when the work is done, folded into another issue or no longer needed, and cite that reason in a comment on the child."},
	{texts.ChildrenFirst, "%s is blocked_by %s, which stands above it on its own Dispatch parent chain, so neither can start until a person takes that link off", "architect",
		goPrompts + "architect-common.md", "neither can start until that link is gone: ask a person with `dispatch ask` to take it off, naming both issues."},
	{texts.CarriedGateOpened, "; any child waiting on it starts", "architect (root)",
		goPrompts + "architect-root.md", "which asks nothing of you, since the daemon has started any child waiting on that gate."},
	{texts.ChildNotRecorded, " Set its repository to one of the project's (dispatch issue-update --repo owner/name), then release it again.", "architect",
		goPrompts + "architect-common.md", "set the child's repository to one both Legion Apps are installed on, spelled as GitHub spells it (`dispatch issue-update --repo owner/name`), or get the App the refusal names installed on the repository it has, then release the child again with `release_children`."},
	{texts.ChildNotRecorded, " Set a repository both Legion Apps are installed on, spelled as GitHub spells it (dispatch issue-update --repo owner/name), then release it again.", "architect",
		goPrompts + "architect-common.md", "set the child's repository to one both Legion Apps are installed on, spelled as GitHub spells it (`dispatch issue-update --repo owner/name`), or get the App the refusal names installed on the repository it has, then release the child again with `release_children`."},
	{texts.ChildReleaseStalled, " Release %s first, then release %s again.", "architect",
		goPrompts + "architect-common.md", "release that issue first, then release the child again once it is recorded in your tree, and tell the controller if it never is."},
	{texts.ChildReleaseStalled, "; inspect the tree before trying again", "architect",
		goPrompts + "architect-common.md", "With `stall: attempts-spent`, the parent's release has not reached the daemon after the tries `attempts` counts, so inspect the tree before you try again."},
	{texts.ChildReleaseStalled, "its parent %s is not recorded: release %s first, then %s", "architect",
		goPrompts + "architect-common.md", "release that issue first, then release the child again once it is recorded in your tree, and tell the controller if it never is."},
	{texts.ChildReleaseStalled, " in Dispatch now: release it again once it is todo)", "architect",
		goPrompts + "architect-common.md", "Otherwise, release the child again once it is `todo`."},
	{texts.WorkerDied, "%s's own architect failed; its notices come to you until it runs again, and the daemon starts it again at %s's next phase start", "architect",
		goPrompts + "architect-common.md", "the child's notices come to you until that architect runs again, and the daemon starts it again at the child's next phase start."},
	{texts.WorkerDied, "the %s does not work %s's phase %s; nothing is held", "architect",
		goPrompts + "architect-common.md", "nothing is held and the phase runs on, so there is nothing to `retry_or_escalate`"},
	{texts.WorkerStalled, "The arguments of a call are never recorded, so they are in no part of this notice.", "architect",
		goPrompts + "architect-common.md", "it names the last call the daemon saw, and never a call's arguments, which the daemon does not record."},
	{texts.WorkerSilent, ". A suspension an operator holds over the claim refuses that start when the outbox runs it", "architect",
		goPrompts + "architect-common.md", "only a suspension an operator holds over the claim still refuses that start, and ask the operator to resume such a claim."},
	{texts.WorkerBackgroundStalled, "The daemon sent that worker nothing and will send it nothing for this: nothing can tell a background job that hung from a long one, so read what its session says it is waiting on and decide whether that work needs a decision of its own", "architect",
		goPrompts + "architect-common.md", "the daemon sends that worker nothing for it, since nothing tells a job that hung from a long one, so read what the session says it waits on and judge whether that work needs a decision of its own."},
	{texts.SessionLost, "It is running a fresh agent, which has read none of its own earlier turns; only what it pushed and committed survives.", "architect",
		goPrompts + "architect-common.md", "the daemon runs the role in a fresh agent, which has read none of its own earlier turns, and only what it pushed and committed survives."},
	{texts.SessionLost, "It is running a fresh agent, which has read none of its own earlier turns; only what it pushed and committed survives.", "controller",
		rolePrompts + "controller-root.md", "the role runs on in a fresh agent, which has read none of its own earlier turns, and only what it pushed and committed survives, so take nothing for it."},
	{texts.SessionForgotten, "its pod's launcher is older than this release and could not look the session up by its id, and Oh My Pi did not resume it by that id.", "architect",
		goPrompts + "architect-common.md", "A `session-forgotten` notice says that a role forgot the session it recorded (`session`) because its pod's launcher, older than the daemon, could not look that session up by its id (`launcher_lookup: failed`), and Oh My Pi did not resume it by that id (`resumed_by_id: false`)"},
	{texts.SessionForgotten, "its pod's launcher is older than this release and could not look the session up by its id, and Oh My Pi did not resume it by that id.", "controller",
		rolePrompts + "controller-root.md", "A `session-forgotten` notice says that a role of the issue forgot the session it recorded (`session`) because its pod's launcher, older than the daemon, could not look that session up by its id (`launcher_lookup: failed`), and Oh My Pi did not resume it by that id (`resumed_by_id: false`)"},
	{texts.SessionForgotten, "The daemon deleted nothing of that session.", "architect",
		goPrompts + "architect-common.md", "the daemon deleted nothing of that session, and it runs the role in a fresh agent"},
	{texts.SessionForgotten, "The daemon deleted nothing of that session.", "controller",
		rolePrompts + "controller-root.md", "the daemon deleted nothing of that session, and the role runs on in a fresh agent"},
	{texts.SessionForgotten, "The %s is running a fresh agent, which has read none of its own earlier turns; only what it pushed and committed survives.", "architect",
		goPrompts + "architect-common.md", "it runs the role in a fresh agent, which has read none of its own earlier turns, and only what it pushed and committed survives."},
	{texts.SessionForgotten, "The %s is running a fresh agent, which has read none of its own earlier turns; only what it pushed and committed survives.", "controller",
		rolePrompts + "controller-root.md", "the role runs on in a fresh agent, which has read none of its own earlier turns, and only what it pushed and committed survives, so take nothing for it."},
	{texts.SessionStoreFault, " Nothing is lost and nothing is charged; the daemon tries again every minute, and no worker of this issue runs until the store answers.", "controller",
		rolePrompts + "controller-root.md", "nothing is lost or charged, the daemon asks the store again every `retry_every`, and no worker of the issue runs until the store answers, so name it in the day's report."},
	{texts.Unslotted, "; the daemon set its phase to done and starts nothing for it; todo with the legion label queues it again", "controller",
		rolePrompts + "controller-root.md", "the daemon set its phase to `done` and starts nothing for it, and `todo` with the `legion` label queues it again, so re-admit nothing."},
	{texts.AwaitingPerson, "%s waits on a person (%s): it keeps its admission slot until they answer", "controller",
		rolePrompts + "controller-root.md", "take nothing for its slot, which it keeps while it waits."},
	{texts.TreeStalled, "Read the tree and decide what it needs. You can ask a role what it waits on. You can move an issue back a phase. You can park the tree.", "controller",
		rolePrompts + "controller-root.md", "read the tree and decide what it needs, as `skill://legion-controller` says: ask a role what it waits on, move an issue back a phase, or ask whether to park the tree."},
	{texts.ArchitectStalled, " That time is counted from the end of pause %d at %s, which held the project.", "controller",
		rolePrompts + "controller-root.md", "`pause-end` is the end of the pause"},
	{texts.ArchitectStalled, " That time is counted from %s, when the wake pacing let out its architect's launch, which pause %d held.", "controller",
		rolePrompts + "controller-root.md", "`launch-let-out` is when the wake pacing let out the architect's launch that the pause held"},
	{texts.ArchitectStalled, " That time is counted from %s, when a turn of its architect first confirmed a task of phase entry %d.", "controller",
		rolePrompts + "controller-root.md", "`entry-confirmed` is when a turn of the architect first confirmed a task of its entry"},
	{texts.CISettled, " Nothing moved on it: decide whether to re-run the checks, wait for them, or fix the code.", "implementer",
		goPrompts + "implementer.md", "That message is yours to act on while the issue is still in a phase you work: re-run a check that failed on itself rather than on your code"},
	{texts.CISettled, " Nothing moved on it: decide whether to re-run the checks, wait for them, or fix the code.", "planner, tester, reviewer, merger",
		goPrompts + "worker-common.md", "whether it is re-run, waited for or fixed is a decision for the roles it reached"},
	{texts.CISettled, " Nothing moved on it: decide whether to re-run the checks, wait for them, or fix the code.", "architect",
		goPrompts + "architect-common.md", "you read it at your next turn: the worker that holds the phase re-runs a red that failed on itself."},
	{texts.CISettled, " Nothing moved on it: decide whether to re-run the checks, wait for them, or fix the code.", "architect",
		goPrompts + "architect-common.md", "A red is yours to decide on — re-run it, wait for it, or have the implementer fix the code —"},
	{texts.CISettled, "; a skipped check ran nothing on this head", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "It names each check GitHub skipped (`skipped`), and its `each_skipped_check: ran_nothing` says once that a skipped check ran nothing on this head, so it is no pass, and it did not fail either."},
	{texts.CISettled, "; a skipped check ran nothing on this head", "architect",
		goPrompts + "architect-common.md", "each check GitHub skipped (`skipped`, beside `each_skipped_check: ran_nothing`, which says once that a skipped check ran nothing on this head, so it is no pass)"},
	{texts.CISettled, "; a skipped check ran nothing on this head", "reviewer",
		goPrompts + "reviewer.md", "each check GitHub skipped (`skipped`, beside `each_skipped_check: ran_nothing`, which says once that a skipped check ran nothing on this head, so it is no pass)"},
	{texts.GateMoved, "and every step the deployment gates now stands open, so the design you are working to is the approved one. A merge the closed gate held is released.", "planner, implementer, tester, reviewer",
		goPrompts + "worker-common.md", "so the design you work to is the approved one, and a merge that the closed gate held is released."},
	{texts.GateMoved, "so the design you are working to may change. Your phase goes on; the merge waits until every gated step stands open again.", "planner, implementer, tester, reviewer",
		goPrompts + "worker-common.md", "the design you work to may change, your phase goes on, and the merge waits until every gated step stands open again."},
	{texts.GateMoved, "so the design you are working to may change. Your phase goes on; the merge waits until every gated step stands open again.", "merger",
		rolePrompts + "merger.md", "so the design this head implements may change: merge nothing, enqueue nothing, and do not complete while the gate is closed."},
	{texts.GateMoved, "so the design you are working to may change. Your phase goes on; the merge waits until every gated step stands open again.", "architect (root)",
		goPrompts + "architect-common.md", "the design may change, the phases go on, and the merge waits until every gated step stands open again."},
	{texts.ApprovedRoundStuck, " The issue waits in reviewing until GitHub reports the head clean, unstable or has_hooks", "reviewer",
		goPrompts + "reviewer.md", "The issue waits in `reviewing` until GitHub reports that head `clean`, `unstable` or `has_hooks`."},
	{texts.ApprovedRoundStuck, "The daemon reads no ruleset and so names no required check: …, which makes the missing check the thing to look for.", "reviewer",
		goPrompts + "reviewer.md", "The daemon reads the base's rules only for a head that conflicts with its base, so a required check that never ran is absent from it, and that missing check is the thing to look for."},
	{texts.ApprovedRoundStuck, "The daemon reads the base's rules only on a conflicting read and so names no required check: …, which makes the missing check the thing to look for.", "reviewer",
		goPrompts + "reviewer.md", "The daemon reads the base's rules only for a head that conflicts with its base, so a required check that never ran is absent from it, and that missing check is the thing to look for."},
	{texts.ApprovedRoundStuck, "; a skipped check ran nothing on this head", "reviewer",
		goPrompts + "reviewer.md", "Its `each_skipped_check: ran_nothing` says once that a skipped check ran nothing on this head, so it is no pass."},
	{texts.ReviewStuck, "; a skipped check ran nothing on this head", "architect",
		goPrompts + "architect-common.md", "Its `skipped` names each check of that head GitHub skipped, and its `each_skipped_check: ran_nothing` says once that a skipped check ran nothing on this head, so it is no pass."},
	{texts.MemoryKill, "The pod's other processes and your session go on. ", "planner, implementer, tester, reviewer, merger",
		rolePrompts + "mechanics/headless.md", "With `cause: guard` or `cause: kernel`, the pod's other processes and your session go on."},
	{texts.MemoryKill, "The pod's other processes and your session go on. ", "architect",
		goPrompts + "architect-common.md", "With `cause: guard` or `cause: kernel`, the pod's other processes and your session go on."},
	{texts.MemoryKill, "Heavy work — a build, a whole test suite, anything that needs much memory — runs where your deployment's instructions say, not in this pod: read skill://legion-worker/references/tools.md before you run it again.", "planner, implementer, tester, reviewer, merger",
		rolePrompts + "mechanics/headless.md", "Before you run heavy work again, read `skill://legion-worker/references/tools.md`: heavy work runs where your deployment's instructions say, not in this pod."},
	{texts.MemoryKill, "Heavy work — a build, a whole test suite, anything that needs much memory — runs where your deployment's instructions say, not in this pod: read skill://legion-worker/references/tools.md before you run it again.", "architect",
		goPrompts + "architect-common.md", "Before you or a subagent of yours runs heavy work again, read `skill://legion-worker/references/tools.md`."},
	{texts.MemoryKill, "It was one of the processes that run your session, not a command you ran. If it was your agent, your turn ended with it, and so did every other process of your role. The daemon then starts the same session again and sends again any task that the turn was running. It counts each such exit of a role's process as a failed launch, and at the limit of failed launches it fails your claim instead. ", "planner, implementer, tester, reviewer, merger",
		rolePrompts + "mechanics/headless.md", "It counts each such exit as a failed launch, and at the limit of failed launches it fails your claim."},
	{texts.MemoryKill, "It was one of the processes that run your session, not a command you ran. If it was your agent, your turn ended with it, and so did every other process of your role. The daemon then starts the same session again and sends again any task that the turn was running. It counts each such exit of a role's process as a failed launch, and at the limit of failed launches it fails your claim instead. ", "architect",
		goPrompts + "architect-common.md", "The daemon then starts the same session again, sends again any task that the turn was running, and counts the exit as a failed launch."},
	{texts.MergeForwardAsk, ", and a second read of GitHub confirmed it.", "implementer",
		goPrompts + "worker-common.md", "the daemon reads GitHub twice, at least a minute apart, and acts only on a conflict both reads report"},
	{texts.MergeForwardAsk, "The next step of the issue is the merge, and GitHub cannot merge a head that conflicts with its base.", "implementer",
		goPrompts + "worker-common.md", "`merge-next`, the issue stands past its review and the merge comes next"},
	{texts.MergeForwardAsk, "The daemon could not read the rules of %s, so it cannot tell if every check that %s requires passed on this head.", "implementer",
		goPrompts + "worker-common.md", "`base-rules-unread`, the daemon could not read the base's rules, so it cannot tell if every check the base requires passed on the head"},
	{texts.MergeForwardAsk, "A ruleset of %s requires a pull request to be current with %s before it merges.", "implementer",
		goPrompts + "worker-common.md", "`base-requires-current`, a ruleset of the base requires a pull request to be current with it"},
	{texts.MergeForwardAsk, "A check that %s requires has not passed on this head, and GitHub runs no checks on a head that conflicts with its base.", "implementer",
		goPrompts + "worker-common.md", "`required-check-not-green`, a check the base requires has not passed on the head, and GitHub runs no check on a head that conflicts with its base."},
	{texts.MergeForwardAsk, " Merge %s forward in the issue workspace, $LEGION_WORKSPACE. Every role of the issue shares that one workspace.", "implementer",
		goPrompts + "implementer.md", "forward in the issue workspace, `$LEGION_WORKSPACE`: every role of the issue shares that one workspace."},
	{texts.MergeForwardAsk, " First do the working-copy check of skill://legion-worker/references/foreign-path.md.", "implementer",
		goPrompts + "implementer.md", "First do the working-copy check of `skill://legion-worker/references/foreign-path.md`."},
	{texts.MergeForwardAsk, " Then merge as skill://legion-worker/references/conflicts-and-rewrites.md says, and push the issue branch.", "implementer",
		goPrompts + "implementer.md", "Then merge as `skill://legion-worker/references/conflicts-and-rewrites.md` says, and push the issue branch."},
	{texts.MergeForwardAsk, " Write no handoff and make no completion for this merge.", "implementer",
		goPrompts + "implementer.md", "Write no handoff and make no completion for this merge."},
	{texts.MergeForwardAsk, " Your own assignment in implementing stands, and so does its completion.", "implementer",
		goPrompts + "implementer.md", "In `implementing`, your own assignment stands, and so does its completion."},
	{texts.MergeForwardHold, " The implementer is asked to merge %s forward in the issue workspace, $LEGION_WORKSPACE, which you share with it.", "tester, reviewer, merger",
		goPrompts + "worker-common.md", "A `merge-forward-hold` says that the implementer is asked to merge the base forward in the issue workspace, `$LEGION_WORKSPACE`, which you share with it"},
	{texts.MergeForwardHold, " Stop writing the workspace now.", "tester, reviewer, merger",
		goPrompts + "worker-common.md", "stop writing the workspace at once"},
	{texts.MergeForwardHold, " Do not merge the pull request, and do not put it in the merge queue.", "merger",
		goPrompts + "merger.md", "Do not merge the pull request, and do not put it in the merge queue, until the new head lands."},
	{texts.MergeForwardHold, " Do not complete your phase at head %s. Complete it at the new head that the daemon names.", "tester, reviewer, merger",
		goPrompts + "worker-common.md", "do not complete your phase at the old head `head` names: complete it at the new head the daemon names."},
	{texts.MergeForwardHold, " Keep your session and the jobs of your turn. The daemon tells you the new head when it lands.", "tester, reviewer, merger",
		goPrompts + "worker-common.md", "Keep your session and the jobs of your turn: nothing of yours is stopped, and the daemon tells you the new head when it lands."},
	{texts.MergeForwardHold, " A result you recorded at head %s stays a result of head %s.", "tester, reviewer, merger",
		goPrompts + "worker-common.md", "A result you recorded at the old head stays a result of that head."},
	{texts.MergeForwardLanded, " Read what changed since the old head: run `git diff %s %s`.", "tester, reviewer, merger",
		goPrompts + "worker-common.md", "read what changed since the old head with `git diff <head> <landed>`, then go on with your work at the new head."},
	{texts.MergeForwardLanded, " For a merge of the base, `git show --remerge-diff %s` shows how the merge resolved the conflict, and `git diff --stat %s^1 %s` lists the files the base brought in.", "tester, reviewer, merger",
		goPrompts + "worker-common.md", "For a merge of the base, `git show --remerge-diff <merge>` shows how the merge resolved the conflict, and `git diff --stat <merge>^ <merge>` lists the files the base brought in."},
	{texts.MergeForwardLanded, " Then go on with your work at the new head.", "tester, reviewer, merger",
		goPrompts + "worker-common.md", "then go on with your work at the new head."},
	{texts.MergeForwardLanded, " Merge the pull request or put it in the merge queue at head %s only once the reviewer's approval of that head is on the pull request.", "merger",
		goPrompts + "merger.md", "merge the pull request or put it in the merge queue at that head only once the reviewer's approval of that head is on the pull request"},
	{texts.MergeForwardReview, " Read what the merge changed since the old head: run `git diff %s %s`.", "reviewer",
		goPrompts + "reviewer.md", "Read what the merge changed since the old head with `git diff <head> <landed>`"},
	{texts.MergeForwardReview, " For a merge of the base, `git show --remerge-diff %s` shows how the merge resolved the conflict, and `git diff --stat %s^1 %s` lists the files the base brought in.", "reviewer",
		goPrompts + "reviewer.md", "for a merge of the base, `git show --remerge-diff <landed>` shows how it resolved the conflict, and `git diff --stat <landed>^ <landed>` lists the files the base brought in."},
	{texts.MergeForwardReview, " Then approve the new head %s by its SHA, with the Legion footer of your review naming that head, or request changes.", "reviewer",
		goPrompts + "reviewer.md", "Then approve the new head by its SHA, with the Legion footer of your review naming that head, or request changes."},
	{texts.MergeForwardReview, " Write no handoff and make no completion for this review.", "reviewer",
		goPrompts + "reviewer.md", "Write no handoff and make no completion for this review."},
	{texts.MergeForwardReview, " Then tell %s what you decided.", "reviewer",
		goPrompts + "reviewer.md", "Then tell the architect what you decided on its role topic, and in `retro` the implementer, in `merging` the merger, each on its own role topic."},
	{texts.UpdateAsk, "2. If you need a scratch file as evidence, put it on Dispatch.", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "if you need a scratch file as evidence, put it on Dispatch, because the pod's disk may not outlast the round. The message's `evidence` names this step."},
	{texts.UpdateAsk, "2. If you need a scratch file as evidence, put it on Dispatch.", "architect",
		goPrompts + "architect-common.md", "if you need a scratch file as evidence, put it on Dispatch, because the pod's disk may not outlast the round. The message's `evidence` names this step."},
	{texts.UpdateAsk, "These things go with the pod when the round ends:", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "When the round ends, the pod goes, whatever runs in it, and these go with it, which the message's `goes_with_pod` names for a pod that does not keep its volume: the turn in progress, each subagent, each background job and each named service that still runs, each commit and each file that you did not push, the pod's test database and the pod's caches."},
	{texts.UpdateAsk, "These things go with the pod when the round ends:", "architect",
		goPrompts + "architect-common.md", "When the round ends, the pod goes, whatever runs in it, and these go with it, which the message's `goes_with_pod` names for a pod that does not keep its volume: the turn in progress, each subagent, each background job and each named service that still runs, each commit and each file that you did not push, the pod's test database and the pod's caches."},
	{texts.UpdateAsk, "Do not start new work now.", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "Start no new work after you answer."},
	{texts.UpdateAsk, "Do not start new work now.", "architect",
		goPrompts + "architect-common.md", "Start no new work after you answer."},
	{texts.UpdateAsk, "It then ends once each role's last report names no subagent and no background job that runs, and no role is in a turn.", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "It does not end early while a role is in a turn or reports background work: it ends once no role is in a turn and each role's last report names no subagent and no background job that runs."},
	{texts.UpdateAsk, "It then ends once each role's last report names no subagent and no background job that runs, and no role is in a turn.", "architect",
		goPrompts + "architect-common.md", "It does not end early while a role is in a turn or reports background work: it ends once no role is in a turn and each role's last report names no subagent and no background job that runs."},
	{texts.UpdateAsk, "Your plugin sends that report at the end of each turn.", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "Your plugin sends that report at the end of each turn."},
	{texts.UpdateAsk, "Your plugin sends that report at the end of each turn.", "architect",
		goPrompts + "architect-common.md", "Your plugin sends that report at the end of each turn."},
	{texts.UpdateAsk, "A subagent or a background job that runs does not delay it either: when the round ends, the pod goes, whatever runs in it.", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "When the round ends, the pod goes, whatever runs in it"},
	{texts.UpdateAsk, "A subagent or a background job that runs does not delay it either: when the round ends, the pod goes, whatever runs in it.", "architect",
		goPrompts + "architect-common.md", "When the round ends, the pod goes, whatever runs in it"},
	{texts.UpdateAsk, "Do these five steps, in this order:", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "Do its five steps in that turn, in this order."},
	{texts.UpdateAsk, "Do these five steps, in this order:", "architect",
		goPrompts + "architect-common.md", "Do its five steps in that turn, in this order."},
	{texts.UpdateAsk, "2. Let each subagent and each background job that you started finish.", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "let each subagent and each background job that you started finish."},
	{texts.UpdateAsk, "2. Let each subagent and each background job that you started finish.", "architect",
		goPrompts + "architect-common.md", "let each subagent and each background job that you started finish."},
	{texts.UpdateAsk, "Stop one that cannot finish before %[2]s.", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "Stop one that cannot finish before the round ends."},
	{texts.UpdateAsk, "Stop one that cannot finish before %[2]s.", "architect",
		goPrompts + "architect-common.md", "Stop one that cannot finish before the round ends."},
	{texts.UpdateAsk, "Then commit and push what it made.", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "Then commit and push what it made."},
	{texts.UpdateAsk, "Then commit and push what it made.", "architect",
		goPrompts + "architect-common.md", "Then commit and push what it made."},
	{texts.UpdateAsk, "3. If you need a scratch file as evidence, put it on Dispatch.", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "if you need a scratch file as evidence, put it on Dispatch, because the pod's disk may not outlast the round. The message's `evidence` names this step."},
	{texts.UpdateAsk, "3. If you need a scratch file as evidence, put it on Dispatch.", "architect",
		goPrompts + "architect-common.md", "if you need a scratch file as evidence, put it on Dispatch, because the pod's disk may not outlast the round. The message's `evidence` names this step."},
	{texts.UpdateAsk, "5. End your turn.", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "end your turn. The message's `then` names this step. If your `legion` tool has no \"ready_for_update\" operation"},
	{texts.UpdateAsk, "5. End your turn.", "architect",
		goPrompts + "architect-common.md", "end your turn. The message's `then` names this step. If your `legion` tool has no \"ready_for_update\" operation"},
	{texts.UpdateAsk, "If a subagent or a background job finishes after you answer, commit and push what it made.", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "If a subagent or a background job finishes after you answer, commit and push what it made."},
	{texts.UpdateAsk, "If a subagent or a background job finishes after you answer, commit and push what it made.", "architect",
		goPrompts + "architect-common.md", "If a subagent or a background job finishes after you answer, commit and push what it made."},
	{texts.UpdateAsk, "Then call `ready_for_update` again with round: \"%[1]s\" before you end that turn.", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "Then call `ready_for_update` again with the same round before you end that turn. The message's `late_background_work` names this rule."},
	{texts.UpdateAsk, "Then call `ready_for_update` again with round: \"%[1]s\" before you end that turn.", "architect",
		goPrompts + "architect-common.md", "Then call `ready_for_update` again with the same round before you end that turn. The message's `late_background_work` names this rule."},
	{texts.UpdateAsk, "A service that you started with a name is not in the report.", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "A service that you started with a name is not in the report."},
	{texts.UpdateAsk, "A service that you started with a name is not in the report.", "architect",
		goPrompts + "architect-common.md", "A service that you started with a name is not in the report."},
	{texts.UpdateAsk, "It does not delay the round, and it goes with the pod.", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "It does not delay the round, and it goes with the pod. The message's `named_services` states that such a service is in no report and holds nothing."},
	{texts.UpdateAsk, "It does not delay the round, and it goes with the pod.", "architect",
		goPrompts + "architect-common.md", "It does not delay the round, and it goes with the pod. The message's `named_services` states that such a service is in no report and holds nothing."},
	{texts.UpdateAsk, "If your `legion` tool has no \"ready_for_update\" operation, do steps 1, 2 and 3 now.", "planner, implementer, tester, reviewer, merger",
		goPrompts + "worker-common.md", "If your `legion` tool has no \"ready_for_update\" operation, do steps"},
	{texts.UpdateAsk, "If your `legion` tool has no \"ready_for_update\" operation, do steps 1, 2 and 3 now.", "architect",
		goPrompts + "architect-common.md", "If your `legion` tool has no \"ready_for_update\" operation, do steps"},
	{texts.MergeQueueRemoval, " Read the pull request and its removals on GitHub, as step 5 of your role says.", "merger",
		rolePrompts + "merger.md", "Read the entries to the queue and the removals from it with this command."},
	{texts.MergeQueueRemoval, " The daemon could not read the pull request from GitHub. So it does not know if GitHub merged it.", "merger",
		rolePrompts + "merger.md", "`unread: true` means that the daemon could not read the pull request from GitHub, so it does not know if GitHub merged it."},
	{texts.VolumeNearFull, "the volume %s's roles share on pod %s was still above its low mark when the sweep ended", "architect",
		goPrompts + "architect-common.md", "The notice tells a past sweep: the volume was above its low mark when that sweep ended, and the notice says nothing of the volume now."},
	{texts.VolumeNearFull, "and the volume grew by %d bytes while the sweep ran", "architect",
		goPrompts + "architect-common.md", "A negative `freedBytes` means that the volume grew by that many bytes while the sweep ran."},
	{texts.VolumeNearFull, "; each size is the apparent size of the files at or under its path, with a file linked under more than one name counted under each name, so the sizes can sum past the bytes the volume uses", "architect",
		goPrompts + "architect-common.md", "Each size is the apparent size of the files at or under its path, with a file linked under more than one name counted under each name, so the sizes can sum past the bytes the volume uses."},
	{texts.VolumeNearFull, "; %q holds at least %d bytes (%d older than an hour, %d older than a day, of what the walk could read): the walk could not make %d of its reads under it, the first at %q", "architect",
		goPrompts + "architect-common.md", "A size with `refused` is a lower bound: `refused` names the first read under its path that the walk did not make, and `refusals` counts those reads."},
	{texts.VolumeNearFull, "; the size walk was cut before it reached every target", "architect",
		goPrompts + "architect-common.md", "`sizesPartial: true` means that a cut ended the size walk before it reached every target."},
	{texts.VolumeNearFull, "the cache trimmer on %s's pod %s has switched itself off and frees nothing more until the pod's roles container restarts (trimmer %q)", "architect",
		goPrompts + "architect-common.md", "`switchedOff: true` means that the trimmer stopped itself after three faults in a row and frees nothing more until the pod's roles container restarts."},
	{texts.VolumeNearFull, ": it made no reading of the volume", "architect",
		goPrompts + "architect-common.md", "its `usedBytes` and `freeBytes` are the trimmer's last reading, or both zero where it made no reading."},
}

// goldenRepositoryRoot is the repository's root, from this package's directory.
const goldenRepositoryRoot = "../../../.."

// goldenDeploymentWords is what a passage may not name: a number, a date or a time (a digit), an
// issue key, or a month.
var goldenDeploymentWords = regexp.MustCompile(`[0-9]|\b(January|February|March|April|May|June|July|August|September|October|November|December)\b`)

// The check c-daemon-texts-are-facts, fourth test: each sentence this change took out of a daemon
// text, for each of its readers, is given by a passage of that reader's prompt, quoted word for
// word on one line of its file. No golden sample holds such a sentence
// outside what R8 keeps and another author's words, and no passage names a number, a date, a time
// or a project of one deployment.
func TestEveryStepTakenOutOfATextIsInItsReadersPrompt(t *testing.T) {
	files := map[string][]string{}
	for _, step := range goldenSteps {
		if _, read := files[step.file]; !read {
			body, err := os.ReadFile(filepath.Join(goldenRepositoryRoot, filepath.FromSlash(step.file)))
			if err != nil {
				t.Fatalf("read %s: %v", step.file, err)
			}
			files[step.file] = strings.Split(string(body), "\n")
		}
		if !slices.ContainsFunc(files[step.file], func(line string) bool { return strings.Contains(line, step.passage) }) {
			t.Errorf("%s does not hold, on one line, the passage that gives the %s the step of %q (%s): %q",
				step.file, step.readers, step.sentence, step.kind, step.passage)
		}
		if found := goldenDeploymentWords.FindString(step.passage); found != "" {
			t.Errorf("the passage of %s for the %s names %q, a number, date, time or project of one deployment: %q",
				step.file, step.readers, found, step.passage)
		}
	}

	held, err := os.ReadFile(goldenTextsPath)
	if err != nil {
		t.Fatalf("read the golden file: %v", err)
	}
	var golden goldenFile
	if err := json.Unmarshal(held, &golden); err != nil {
		t.Fatalf("decode the golden file: %v", err)
	}
	if len(golden.Samples) == 0 {
		t.Fatal("the golden file holds no sample")
	}
	for _, sample := range golden.Samples {
		said := goldenSaid(t, sample)
		for _, step := range goldenSteps {
			if goldenSentence(step.sentence).MatchString(said) {
				t.Errorf("%s holds %q, the step of %s that the %s's prompt gives now (%s)", sample.id(), step.sentence, step.kind, step.readers, step.file)
			}
		}
	}
}

// goldenSaid is what a golden sample says in the daemon's own words: a task's text but the spans
// of R1's blocks and of the sentences R8 keeps, or a payload's every string but those its words
// object names, which are another author's.
func goldenSaid(t *testing.T, sample goldenSample) string {
	t.Helper()
	if sample.Form == texts.Task {
		units := utf16.Encode([]rune(sample.Text))
		var said []uint16
		at := 0
		for _, span := range sample.Spans {
			said = append(said, units[at:span.Start]...)
			at = span.End
		}
		said = append(said, units[at:]...)
		return string(utf16.Decode(said))
	}
	var payload any
	if err := json.Unmarshal(sample.Payload, &payload); err != nil {
		t.Fatalf("decode the payload of %s: %v", sample.id(), err)
	}
	if text, ok := payload.(string); ok {
		return text
	}
	values := map[string]string{}
	goldenFlatten(payload, "", values)
	words := map[string]bool{}
	for path := range values {
		if rest, ok := strings.CutPrefix(path, "words."); ok {
			field, _, _ := strings.Cut(rest, ".by")
			field, _, _ = strings.Cut(field, ".at")
			field, _, _ = strings.Cut(field, ".rest")
			field, _, _ = strings.Cut(field, ".more")
			words[field] = true
		}
	}
	var said strings.Builder
	for _, path := range slices.Sorted(goldenKeys(values)) {
		if strings.HasPrefix(path, "words.") || words[path] {
			continue
		}
		said.WriteString(values[path])
		said.WriteString("\n")
	}
	return said.String()
}

// goldenVerb is a format verb, with or without an argument index such as %[2]s, or an elision of
// a sentence of the table.
var goldenVerb = regexp.MustCompile(`%(?:\[[0-9]+\])?[sdqv]|…`)

// goldenSentence is a sentence of the table as a pattern: its fixed parts word for word, trimmed
// of the spaces at its ends, with any text where a format verb or an elision stands.
func goldenSentence(sentence string) *regexp.Regexp {
	parts := goldenVerb.Split(strings.TrimSpace(sentence), -1)
	for i, part := range parts {
		parts[i] = regexp.QuoteMeta(part)
	}
	return regexp.MustCompile(strings.Join(parts, `(?s:.*?)`))
}
