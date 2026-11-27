T0's public example (packages/pi-legion/scripts/skill-scenarios/testdata/heldout-case-example.json)
with every typed world part written onto its cases' worlds, each at a value that leaves the
example's four traces answered as they record. Its cases' traces, actions and rubrics are the
example's.

P05's world (ACME-12, a child of ACME-10, implementing) holds:
  daemon.claim      the implementer's claim on ACME-12 in the tree of ACME-10, with the session
                    ses-acme-12-implementer, which no record of the world names (no write is the
                    role's own)
  daemon.issue      ACME-12's record: in the tree of ACME-10 under it, generation 1, no linger
  daemon.entry      the implementer's row on entry 1, the claim serving entry 1, no owed task
  daemon.architect  no architect claim of ACME-12 (null)
  daemon.records    ACME-10, the tree's root, admitted, in_progress, with no architect claim; and
                    ACME-13, a done member of its tree
  daemon.admission  cap 4 (the daemon's default), ACME-12 active, nothing waiting or blocked
  daemon.gate       ACME-10's spec step: spec.md at version 1, approved at 1, named by the id
                    6c174565-c501-58c9-b35a-149c49ff8ffa
  dispatch.issue.labels  legion
  dispatch.issues   ACME-13, done, under ACME-10, rank 1, labelled ux, no link, no claim
  dispatch.steps    the planner's passed run of step 1 of the default task
  github.settings   default_branch main, allow_merge_commit false
  github.branchRules  main's one rule, non_fast_forward
  github.pullRequests  #5, closed, on legion/ACME-9, with its merge state (DIRTY), no auto-merge
                    request, its node id, its diff and its merge answer
P10's pull request #77 gains its merge state (CLEAN), no auto-merge request, its node id and its
diff.

Made on 2026-11-27 with jq 1.7, from the example's file, then formatted as this repository's JSON is
(./node_modules/.bin/biome format --write):

  jq -f typed-parts.jq packages/pi-legion/scripts/skill-scenarios/testdata/heldout-case-example.json

where typed-parts.jq is:

.cases |= map(
  if .id == "P05" then
    .world.daemon += {
      claim: {issue: "ACME-12", tree: "ACME-10", session: "ses-acme-12-implementer"},
      issue: {tree: "ACME-10", parent: "ACME-10", generation: 1, lingerUntil: null},
      entry: {phase: 1, turn: 1, owedTask: null, movedBack: false},
      architect: null,
      records: [
        {key: "ACME-10", tree: "ACME-10", parent: null, phase: "admitted", status: "in_progress", generation: 1, architect: null},
        {key: "ACME-13", tree: "ACME-10", parent: "ACME-10", phase: "done", status: "done", generation: 1, architect: null}
      ],
      admission: {cap: 4, active: ["ACME-12"], waiting: [], blocked: []},
      gate: [{step: "spec", document: "spec.md", current: 1, approved: 1, artifactId: "6c174565-c501-58c9-b35a-149c49ff8ffa"}]
    }
    | .world.dispatch.issue.labels = ["legion"]
    | .world.dispatch.issues = [{key: "ACME-13", status: "done", parent: "ACME-10", rank: 1, labels: ["ux"], blockedBy: [], claimedBy: null}]
    | .world.dispatch.steps = [{task: "default", step: 1, kind: "step", role: "planner", result: "passed", startedAt: "2030-01-07T13:00:00Z", endedAt: "2030-01-07T13:30:00Z"}]
    | .world.github.settings = {default_branch: "main", allow_merge_commit: false}
    | .world.github.branchRules = [{branch: "main", rules: [{type: "non_fast_forward"}]}]
    | .world.github.pullRequests += [{number: 5, state: "closed", draft: false,
        head: {ref: "legion/ACME-9", sha: "1f3a5c7e9b2d4f6a8c0e1b3d5f7a9c2e4b6d8f0a"}, base: "main",
        title: "An abandoned change", body: "", mergeCommit: null, reviews: [], reviewThreads: [], checks: [],
        mergeStateStatus: "DIRTY", autoMergeRequest: null, nodeId: "PR_kwDOexample5",
        diff: "diff --git a/README.md b/README.md\n", mergeResult: {merged: false, message: "Pull Request is not mergeable"}}]
  elif .id == "P10" then
    .world.github.pullRequests |= map(. + {mergeStateStatus: "CLEAN", autoMergeRequest: null,
      nodeId: "PR_kwDOexample77", diff: "diff --git a/step.go b/step.go\n"})
  else . end)

What the replay must show. TEARDOWN_EFFECTS_EVIDENCE_DIR=<dir> bash scripts/e2e/teardown-effects.sh
--controls --cases packages/daemon/internal/daemon/testdata/teardown-effects/typed-parts-cases.json
replays the same four traces as on the example (P05 good and bad, P10 good and bad):
  - each start seeds every part and reads each back, so no trace fails its setup: no replay.json
    names a world part (no "world.daemon", "world.dispatch" or "world.github" in a HARNESS reason);
  - each of the four replay.json has verdict ok and an effects.json beside it, each daemon call
    answered as its trace records, at the example's seq;
  - every control teardownEffectsPromised promises for the example executes, and daemon-answers.json
    is written, as on the example (controls/).
A HARNESS verdict whose reason names a part is a part lane D does not seed as the world states it.
