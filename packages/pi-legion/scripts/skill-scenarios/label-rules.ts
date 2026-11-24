// label-rules.ts: every sentence an ablation label cuts or keeps in the role texts it reads.

import type { Role } from "./role-texts";

export type Tree = "base" | "head";

export interface Entry {
  readonly file: string;
  readonly has?: string;
  readonly fenced?: true;
  readonly at?: Tree;
  readonly cut?: string;
  readonly keep?: string;
  /** For a `cut` that puts the old words back: words of the old sentence that the label's checkout
   * must hold in this file. A label that deletes the sentence instead measures no control, since
   * nothing then tells its agent the old rule (rule-search.ts `cut`). */
  readonly restore?: string;
}

export interface Rule {
  /** The roles whose texts the label's search reads: one role, or each role a label's runs drive
   * when one checkout serves the scenarios of two. */
  readonly roles: readonly Role[];
  readonly rule: string;
  readonly patterns: readonly RegExp[];
  readonly entries: readonly Entry[];
  /** Words the label writes into its roles' tasks, before the task's `Pull request:` sentence
   * (rig.sh's task_words): words the daemon of the release before wrote there, which the label
   * puts back beside its cuts, since a checkout holds the prompts and never the task. */
  readonly taskWords?: string;
}

const LATER_FAIL: readonly RegExp[] = [
  /pass and (a )?fail|fail and (a )?pass|after a pass|later (fail|pass|result)|fail recorded after/i,
  /retract|not known|do not wait|no wait\b|wait for a later/i,
  /\bfail(s|ed)?\b[^.]{0,80}\bpass(es)?\b|\bpass(es)?\b[^.]{0,80}\bfail(s|ed)?\b/i,
];

const GUIDE_TWO_ROLES: readonly Entry[] = [];

const GUIDE_LATER_FAIL: readonly Entry[] = [
  {
    file: "skills/legion-controller/references/finding-asks.md",
    has: "Close an open finding ask that turned moot with `dispatch resolve-ask --kind retracted`",
    keep: "A finding ask's retraction, not a check result.",
  },
];

const ENTRIES_TWO_ROLES: readonly Entry[] = [
  {
    file: "packages/daemon/internal/prompts/go/reviewer.md",
    has: "A completion that names the handoff record you named for your previous round",
    keep: "Each round's handoff, not a Dispatch check.",
  },
  {
    file: "packages/daemon/internal/prompts/go/worker-common.md",
    has: "One write is the exception",
    keep: "A forward merge's writer, not a Dispatch check.",
  },
  {
    file: "packages/daemon/internal/prompts/go/worker-common.md",
    has: "for the implementer, a red-test commit the tester left there",
    keep: "Whose commits the implementer's push carries, not a Dispatch check.",
  },
  {
    file: "packages/daemon/internal/prompts/go/worker-common.md",
    has: "It keeps each role's session",
    keep: "A released pod's state, not a Dispatch check.",
  },
  {
    file: "packages/daemon/internal/prompts/go/worker-common.md",
    has: "It does not end early while a role is in a turn or reports background work",
    keep: "An update round's end, not a Dispatch check.",
  },
  {
    file: "packages/daemon/internal/prompts/go/worker-common.md",
    has: "It keeps nothing else of the disk",
    keep: "A released pod's disk, not a Dispatch check.",
  },
  {
    file: "packages/daemon/internal/prompts/go/worker-common.md",
    has: "A kept volume keeps the files of that list that are on the volume",
    keep: "A kept microVM volume's disk, not a Dispatch check.",
  },
  {
    file: "packages/daemon/internal/prompts/go/worker-common.md",
    has: "The implementer, or in `merging` the merger, is asked to merge the base forward",
    keep: "A forward merge's assignment, not a Dispatch check.",
  },
  {
    file: "packages/daemon/internal/prompts/go/worker-common.md",
    has: "The merger pushes only a merge of the base forward",
    keep: "The merger's push and its read of the pull request's head, not a Dispatch check.",
  },
  {
    file: "packages/daemon/internal/prompts/roles/core/reviewer.md",
    has: "from the implementer and, where a tester worked the change, from the tester",
    cut: "Remove the two-role requirement.",
  },
  {
    file: "packages/daemon/internal/prompts/roles/core/reviewer.md",
    has: "results from CI stand for both roles",
    cut: "Remove the two-role comparison from the CI sentence.",
  },
  {
    file: "packages/daemon/internal/prompts/roles/mechanics/headless.md",
    has: "A clear defect in code this tree touches or depends on is fixed in this tree's own pull request",
    keep: "Who fixes a defect a role finds, and who it is sent to, not the roles a check's pass needs.",
  },
  {
    file: "packages/daemon/internal/prompts/roles/mechanics/headless.md",
    has: "Under `runtime: kubernetes` the daemon releases an issue pod",
    keep: "A released pod, not a Dispatch check.",
  },
  {
    file: "packages/daemon/internal/prompts/roles/reviewer.md",
    has: "from the implementer and, where the task has a tester step, from the tester",
    cut: "Remove the two-role requirement.",
  },
  {
    file: "skills/legion-architect/references/acceptance.md",
    has: "The tester sizes that round like any other",
    keep: "Who runs a parent's last round, not the roles a check's pass needs.",
  },
  {
    file: "skills/legion-architect/references/plan.md",
    has: "A `planner` step may stand at any place",
    keep: "The places the steps may stand in, not the roles a check's pass needs.",
  },
  {
    file: "skills/legion-architect/references/decomposition.md",
    has: "Every child names the running system its criteria are proven on",
    keep: "The running system a child's criteria are proven on, not a Dispatch check's pass.",
  },
  {
    file: "skills/legion-architect/references/plan.md",
    has: "This release gives each role one agent for each issue",
    keep: "Each role's agent, not a Dispatch check.",
  },
  {
    file: "skills/legion-architect/references/plan.md",
    has: "Only the implementer records results against the checks, and no other step runs them again",
    keep: "Who records results in a plan the architect writes without a tester step, not the roles the reviewer's gate needs for a pass.",
  },
  {
    file: "skills/legion-controller/references/daily-report.md",
    has: "Phase workers nothing will prompt",
    keep: "A controller's silent-worker report, not a Dispatch check.",
  },
  {
    file: "skills/legion-controller/references/escalation.md",
    has: "The daemon replaces a pod whose configuration moved only after an update round",
    keep: "A pod update, not a Dispatch check.",
  },
  {
    file: "skills/legion-retro/SKILL.md",
    has: "Any other check with no pass standing, or a check with no `ci_check` whose only evidence is a unit suite",
    keep: "The destination of a failure, not who makes a pass.",
  },
  {
    file: "skills/legion-retro/SKILL.md",
    has: "With no reviewer step, also record step 1's bot threads in `Threads`",
    keep: "Retro's update of the pull request body with no reviewer step, not a Dispatch check.",
  },
  {
    file: "skills/legion-retro/SKILL.md",
    fenced: true,
    keep: "A format or command in a fenced block.",
  },
  {
    file: "skills/legion-worker/SKILL.md",
    has: "name: legion-worker",
    keep: "The skill description.",
  },
  {
    file: "skills/legion-worker/SKILL.md",
    has: "They override this skill, except",
    keep: "The implementer's own result, not a two-role gate.",
  },
  {
    file: "skills/legion-worker/SKILL.md",
    has: "With no tester step, the implementer's evidence serves",
    cut: "Remove the implementer's evidence standing alone on a task with no tester step, the other half of the two-role requirement.",
  },
  {
    file: "skills/legion-worker/references/ci-checks.md",
    has: "What each role does",
    keep: "A section heading, not a Dispatch rule.",
  },
  {
    file: "skills/legion-worker/references/ci-checks.md",
    has: "Each tester and reviewer step sends it to the architect as a finding",
    keep: "Who sends a check's wrong words to the architect, not who makes a pass.",
  },
  {
    file: "skills/legion-worker/references/ci-checks.md",
    has: "It proves only that the job passed there",
    keep: "What a CI job's pass proves and who proves the surface CI cannot run, not the roles a check with no `ci_check` needs for a pass.",
  },
  {
    file: "skills/legion-worker/references/merge-gate.md",
    has: "With no earlier tester step, the implementer's evidence for each is what you read",
    cut: "Delete the reviewer's read of the implementer's evidence alone on a task with no tester step, the other half of the two-role requirement.",
  },
  {
    file: "skills/legion-worker/references/pr-body.md",
    has: "With no reviewer step, at retro the implementer updates `Not proven / risk`",
    keep: "Who updates the pull request body with no reviewer step, not a Dispatch check.",
  },
  {
    file: "skills/legion-worker/references/review-threads.md",
    has: "The implementer records each thread in the PR body's `Threads` section",
    keep: "Review-thread recording, not a Dispatch check.",
  },
  {
    file: "skills/legion-worker/references/conflicts-and-rewrites.md",
    has: "The merger records none itself: it asks the implementer, and the tester where the task has a tester step",
    keep: "Whom the merger asks to record results at its merge head, not the reviewer's gate on a pass.",
  },
  ...GUIDE_TWO_ROLES,
];

const ENTRIES_LATER_FAIL: readonly Entry[] = [
  {
    file: "packages/daemon/internal/prompts/go/reviewer.md",
    has: "`legion-verdict` is the one required check you do not wait for",
    keep: "The check the daemon writes from the reviewer's own verdict, not a check result.",
  },
  {
    file: "packages/daemon/internal/prompts/roles/core/reviewer.md",
    has: "That answer leaves out a pass at another commit",
    cut: "Restore the old later-fail rule.",
    restore:
      "on a check with no `ci_check` a fail recorded after a pass leaves that pass standing until someone retracts it",
  },
  {
    file: "packages/daemon/internal/prompts/roles/core/reviewer.md",
    has: "Until that run concludes, wait for it by its name, unless that head's line reads not known",
    cut: "Remove the pass-and-fail exception from the wait.",
  },
  {
    file: "packages/daemon/internal/prompts/roles/core/reviewer.md",
    has: "A pass and a fail at one head, from any role, leave that head not known",
    cut: "Restore the prior CI-only rule.",
    restore: "A pass and a fail from CI at one head leave no pass standing there.",
  },
  {
    file: "packages/daemon/internal/prompts/roles/reviewer.md",
    has: "Wait for a `ci_check` run still going or unread, unless the head's line reads not known",
    cut: "Remove the pass-and-fail exception from the wait.",
  },
  {
    file: "packages/daemon/internal/prompts/roles/reviewer.md",
    has: "A pass and a fail at one head leave it not known, whoever recorded them",
    cut: "Remove the reviewer rule.",
  },
  {
    file: "packages/daemon/internal/prompts/roles/reviewer.md",
    has: "A pass and a fail at any open head make the check not known",
    cut: "Remove the reviewer rule.",
  },
  {
    file: "skills/legion-architect/SKILL.md",
    has: "Act on each delivered wake by its row",
    keep: "Wake routing, not a check result.",
  },
  {
    file: "skills/legion-architect/SKILL.md",
    has: "A root's questions go on the root",
    keep: "A root question, not a check result.",
  },
  {
    file: "skills/legion-retro/SKILL.md",
    has: "A `ci_check` check has no pass standing at that head when",
    keep: "A CI-proved check's result, not the no-ci-check rule.",
  },
  {
    file: "skills/legion-retro/SKILL.md",
    has: "A run that has not concluded, or one GitHub could not be read for, is neither a pass nor a fail yet",
    keep: "A CI run still going, which is neither a pass nor a fail, not a pass and a fail at one head.",
  },
  {
    file: "skills/legion-worker/references/ci-checks.md",
    fenced: true,
    keep: "A format or command in a fenced block.",
  },
  {
    file: "skills/legion-worker/references/pr-body.md",
    fenced: true,
    keep: "A format or command in a fenced block.",
  },
  {
    file: "skills/thermonuclear-deep-review/SKILL.md",
    has: "Order, not coverage: after a pipeline sanitizes",
    keep: "A security-review question, not a Dispatch check.",
  },
  {
    file: "skills/thermonuclear-deep-review/SKILL.md",
    has: "`untrusted-input`",
    keep: "A security-review table row, not a Dispatch check.",
  },
  ...GUIDE_LATER_FAIL,
];

const ENTRIES_ARCHITECT_LATER_FAIL: readonly Entry[] = [
  {
    file: "packages/daemon/internal/prompts/roles/architect-root.md",
    has: "The person's answer, resolution or reply on any ask of the root",
    keep: "A person's answer, not a check result.",
  },
  {
    file: "packages/daemon/internal/prompts/roles/architect-root.md",
    has: "A pass and a fail at any open head make the check not known",
    cut: "Restore the architect's old later-fail rule.",
    // The old words of skills/legion-architect/SKILL.md at d2499ad82d, which the check's negative
    // control restores: a label that only deletes the rule tells its architect no old rule.
    restore:
      "On a check with no `ci_check`, a fail recorded after a pass leaves that pass standing until someone retracts it",
  },
  {
    file: "packages/daemon/internal/prompts/roles/architect-root.md",
    has: "To give a check to CI, write a new check block under a new id",
    keep: "A CI-check authoring rule, not the pass-and-fail rule.",
  },
  {
    file: "skills/legion-architect/SKILL.md",
    has: "Act on each delivered wake by its row",
    keep: "Wake routing, not a check result.",
  },
  {
    file: "skills/legion-architect/SKILL.md",
    has: "A root's questions go on the root",
    keep: "A root question, not a check result.",
  },
  {
    file: "skills/legion-retro/SKILL.md",
    has: "A `ci_check` check has no pass standing at that head when",
    keep: "A CI-proved check's result, not the architect rule.",
  },
  {
    file: "skills/legion-retro/SKILL.md",
    has: "A run that has not concluded, or one GitHub could not be read for, is neither a pass nor a fail yet",
    keep: "A CI run still going, which is neither a pass nor a fail, not a pass and a fail at one head.",
  },
  {
    file: "skills/legion-worker/references/ci-checks.md",
    fenced: true,
    keep: "A format or command in a fenced block.",
  },
  {
    file: "skills/legion-worker/references/pr-body.md",
    fenced: true,
    keep: "A format or command in a fenced block.",
  },
  {
    file: "skills/thermonuclear-deep-review/SKILL.md",
    has: "Order, not coverage: after a pipeline sanitizes",
    keep: "A security-review question, not a Dispatch check.",
  },
  {
    file: "skills/thermonuclear-deep-review/SKILL.md",
    has: "`untrusted-input`",
    keep: "A security-review table row, not a Dispatch check.",
  },
  ...GUIDE_LATER_FAIL,
];

// A sentence that sends a reader to `dispatch results`, or to the state of a check at a head, which
// that command prints on the head's line under each check: a pass standing there, the `not known` of
// a pass and a fail at one head, the result it derives for a check CI proves, the state of each check
// that the daemon's move follows, and what "that command" prints, in a sentence after one that named
// it.
const STATES_READ: readonly RegExp[] = [
  /dispatch results/i,
  /\bline (of|for) (your|its|the|this|that) head\b|\bhead's line\b|\bstate line\b|\bpass_stands\b|\ba pass (stands|standing)\b|\bpass stands for\b/i,
  /\bnot known\b|\bderived result\b|\b(that|the) command (still )?(prints|derives)\b/i,
  /\bstates? of (each|a|the|every) check\b|\beach check's state\b|\bstates? at (the|that|this|its|your) (\w+ )?head\b/i,
];

const ENTRIES_STATES_READ: readonly Entry[] = [
  {
    file: "packages/daemon/internal/prompts/go/architect-common.md",
    has: "`dispatch results --issue <issue>` at the head that step passed",
    cut: "Remove the read of the head's results from the evidence of the acceptance.",
  },
  {
    file: "packages/daemon/internal/prompts/go/architect-common.md",
    has: "From the tester's phase, its `reason` says what the testing move did",
    cut: "Delete what a tester's notice says of the testing move, which each check's state at the tested head decides.",
  },
  {
    file: "packages/daemon/internal/prompts/go/architect-common.md",
    has: "so read `dispatch results` at the new head before you accept",
    cut: "Delete the read of the new head's results before an acceptance past review.",
  },
  {
    file: "packages/daemon/internal/prompts/go/reviewer.md",
    has: "evidence of each check is the line of your head in `dispatch results`",
    cut: "Delete the sentence on the reviewer's evidence.",
  },
  {
    file: "packages/daemon/internal/prompts/go/worker-common.md",
    has: "the command prints the version it recorded",
    keep: "What `dispatch handoff-write` prints, not the state of a check.",
  },
  {
    file: "packages/daemon/internal/prompts/go/worker-common.md",
    has: "without `--version` the command prints the phase's newest version",
    keep: "What `dispatch handoff-read` prints, not the state of a check.",
  },
  {
    file: "packages/daemon/internal/prompts/roles/architect-root.md",
    has: "You then read `dispatch results --issue <issue>` at the head that step passed",
    cut: "Remove the read of the head's results from the evidence of the acceptance.",
  },
  {
    file: "packages/daemon/internal/prompts/roles/architect-root.md",
    has: "each needs a pass standing at that head, which `dispatch results --issue <issue>` shows",
    cut: "Delete the acceptance's gate on each check's pass at the head.",
  },
  {
    file: "packages/daemon/internal/prompts/roles/architect-root.md",
    has: "A pass and a fail at any open head make the check not known",
    cut: "Delete the architect's rule for a head whose line reads not known.",
  },
  {
    file: "packages/daemon/internal/prompts/roles/architect-root.md",
    has: "At acceptance, read each CI-proved check's derived result",
    cut: "Delete the read of each CI-proved check's derived result at the acceptance.",
  },
  {
    file: "packages/daemon/internal/prompts/roles/architect-root.md",
    has: "The tester's completion carries no verdict: the daemon decides the move by the state of each check",
    cut: "Remove the testing move by each check's state at the tested head from the sentence on the tester's completion.",
  },
  {
    file: "packages/daemon/internal/prompts/roles/core/reviewer.md",
    has: "Verify as forge facts, never from reports",
    cut: "Remove each check's pass at the head and that head's line from the facts the reviewer verifies.",
  },
  {
    file: "packages/daemon/internal/prompts/roles/core/reviewer.md",
    has: "That answer leaves out a pass at another commit",
    cut: "Delete what the head's line leaves out.",
  },
  {
    file: "packages/daemon/internal/prompts/roles/core/reviewer.md",
    has: "results from CI stand for both roles: the one that command derives from its CI run",
    cut: "Remove the results `dispatch results` prints under a check CI proves from the sentence on that check.",
  },
  {
    file: "packages/daemon/internal/prompts/roles/core/reviewer.md",
    has: "Until that run concludes, wait for it by its name, unless that head's line reads not known",
    cut: "Remove the head's line from the wait for a CI run.",
  },
  {
    file: "packages/daemon/internal/prompts/roles/core/reviewer.md",
    has: "A pass and a fail at one head, from any role, leave that head not known",
    cut: "Delete the reviewer's rule for a head whose line reads not known.",
  },
  {
    file: "packages/daemon/internal/prompts/roles/reviewer.md",
    has: "Wait for a `ci_check` run still going or unread, unless the head's line reads not known",
    cut: "Remove the head's line from the wait for a CI run.",
  },
  {
    file: "packages/daemon/internal/prompts/roles/reviewer.md",
    has: "Your `approve` is gated by the `before_merge` checks of this issue's own Verification document",
    cut: "Delete the gate of `approve` on each check's pass at the reviewed head.",
  },
  {
    file: "packages/daemon/internal/prompts/roles/reviewer.md",
    has: "A pass and a fail at one head leave it not known, whoever recorded them",
    cut: "Delete the reviewer's rule for a head whose line reads not known.",
  },
  {
    file: "packages/daemon/internal/prompts/roles/reviewer.md",
    has: "A pass and a fail at any open head make the check not known",
    cut: "Delete the reviewer's rule for a head whose line reads not known.",
  },
  {
    file: "skills/legion-architect/SKILL.md",
    has: "Accept only when a pass stands for every `before_merge` check at the head the step before your acceptance passed",
    cut: "Remove each check's pass at the last step's head from the acceptance.",
  },
  {
    file: "skills/legion-architect/references/plan.md",
    has: "A reviewer step approves only once a pass stands for every `before_merge` check at the head",
    keep: "What leaving a step out of a plan means, with the gate's own facts in the reviewer's role part.",
  },
  {
    file: "skills/legion-architect/references/acceptance.md",
    has: "whether a pass stands for each `before_merge` check at the head the step before your acceptance passed",
    cut: "Delete item 2 of the acceptance's list.",
  },
  {
    file: "skills/legion-architect/references/acceptance.md",
    has: "Read each `ci_check` result in `dispatch results`",
    cut: "Remove the read of each `ci_check` result from the last round.",
  },
  {
    file: "skills/legion-architect/references/acceptance.md",
    has: "Each check needs a pass at that head; one in another repository",
    keep: "A parent's last round, once its children close, not the acceptance of an issue at its own approved head.",
  },
  {
    file: "skills/legion-retro/SKILL.md",
    has: "**Read the evidence**: the issue, `dispatch results --issue <issue>`",
    keep: "Retro's read after the task's last step, not a decision of the reviewer or the architect.",
  },
  {
    file: "skills/legion-retro/SKILL.md",
    has: "A check carrying `ci_check` is CI's: its derived result is the pass it needs",
    keep: "Retro's read after the task's last step, not a decision of the reviewer or the architect.",
  },
  {
    file: "skills/legion-retro/SKILL.md",
    has: "A `ci_check` check has no pass standing at that head when its derived result is a fail",
    keep: "Retro's read after the task's last step, not a decision of the reviewer or the architect.",
  },
  {
    file: "skills/legion-retro/SKILL.md",
    has: "A `ci_check` check whose derived result is a fail is the merger's",
    keep: "Retro's read after the task's last step, not a decision of the reviewer or the architect.",
  },
  {
    file: "skills/legion-retro/SKILL.md",
    fenced: true,
    keep: "A format or command in a fenced block.",
  },
  {
    file: "skills/legion-worker/references/ci-checks.md",
    has: "reads the named run at the head of the issue's pull request and prints the one result it derives",
    keep: "What the command prints for a check CI proves, not where a role decides.",
  },
  {
    file: "skills/legion-worker/references/ci-checks.md",
    has: "The next role that writes the body reads `dispatch results` again",
    keep: "Who writes the PR body's line again, not where a role decides.",
  },
  {
    file: "skills/legion-worker/references/ci-checks.md",
    has: "A `ci_check` check goes in `failures` only when its derived result is a fail",
    keep: "What the tester's handoff lists in `failures`, not where the reviewer or the architect decides.",
  },
  {
    file: "skills/legion-worker/references/ci-checks.md",
    fenced: true,
    keep: "A format or command in a fenced block.",
  },
  {
    file: "skills/legion-worker/references/merge-gate.md",
    has: "Read CI on GitHub and the issue's results with `dispatch results --issue <issue>`, never from a handoff.",
    cut: "Delete item 1 of the reviewer's list.",
  },
  {
    file: "skills/legion-worker/references/pr-body.md",
    fenced: true,
    keep: "A format or command in a fenced block.",
  },
];

export const RULES: Record<string, Rule> = {
  "ablate-two-roles": {
    roles: ["reviewer"],
    rule: "A check with no `ci_check` has a pass at a head only from the implementer and from the tester both.",
    patterns: [
      /implementer[^.]*\btester|tester[^.]*\bimplementer/i,
      /\b(both|two|which|each) roles?\b|implementer.s own|from the implementer\b|from the tester\b/i,
      /implementer records|tester records|records? (its|their|your) own/i,
    ],
    entries: ENTRIES_TWO_ROLES,
  },
  "ablate-later-fail": {
    roles: ["reviewer"],
    rule: "A pass and a fail at one head leave it not known, whoever recorded them: request changes, with no wait.",
    patterns: LATER_FAIL,
    entries: ENTRIES_LATER_FAIL,
  },
  "ablate-architect-later-fail": {
    roles: ["architect"],
    rule: "A pass and a fail at one head leave it not known and the check's answer never yes: send the issue back, with no wait.",
    patterns: LATER_FAIL,
    entries: ENTRIES_ARCHITECT_LATER_FAIL,
  },
  "ablate-states-read": {
    roles: ["reviewer", "architect"],
    rule: "The reviewer and the architect each decide by the state of each check at their head, as `dispatch results` prints it, and no task names a verdict of the tester.",
    patterns: STATES_READ,
    entries: ENTRIES_STATES_READ,
    taskWords: "Tester's verdict: pass.",
  },
};
