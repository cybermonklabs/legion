// The world of the heads scenarios (heads-scenarios.txt) on a run's scratch Dispatch, its GitHub
// stand-in and its NATS: the modes `seed.ts heads-issue` and `seed.ts heads-events`, whose usage
// seed.ts's header gives, and which seed.ts runs. e2e/api.ts reads its port when it loads, as
// seed.ts's header says.
import { readFileSync, writeFileSync } from "node:fs";
import { connect, StringCodec } from "nats";
import { z } from "zod";
import {
  baseUrl,
  createHandoff,
  createIssue,
  createIssueArtifact,
  e2eAgentToken,
} from "../../../dispatch/e2e/api";
import { harnessPorts } from "../../../dispatch/e2e/harness-ports";
import { headsCases } from "./heads-scenarios";
import { RIG_WORLD, type RigCIJob } from "./rig-world";
import { agentPost, IMPLEMENTER } from "./seed-requests";

/** The repository of the heads scenarios' world and its default branch, rig-world.json's. */
const HEADS_REPO = RIG_WORLD.repository;
const HEADS_BASE = RIG_WORLD.defaultBranch;
/** The job of k1's `ci_check` in the case ci-only, whose check run the GitHub stand-in serves, and
 * the job of k1's `ci_check` in ci-pass-and-fail, whose third attempt at #702's head is still going
 * while its first two recorded a pass and then a fail there (rig.sh's ci_job_results). Each is
 * rig-world.json's, which the agent's gh stand-in (rig.sh's heads_routes) serves too. */
const CI_JOB = RIG_WORLD.ciJobs["ci-only"];
const CI_RUNNING_JOB = RIG_WORLD.ciJobs["ci-pass-and-fail"];
/** The job of k2's `ci_check` in the ci-check-fail worlds: its failed run, at #702's head in
 * ci-check-fail and at #703's in ci-check-fail-other-head, and its passed run at #702's head in the
 * second. */
const CI_FAIL_JOB = RIG_WORLD.ciJobs["ci-check-fail"];
const CI_PASS_JOB = RIG_WORLD.ciJobs["ci-check-pass"];
/** Whether a world is one of a failed CI check: of reviewer-own-head, named by its case alone, or
 * of architect-pass-and-fail, by the scenario's name and the case. */
const ciCheckWorld = (world: string) => /(^|-)ci-check-fail(-other-head)?$/.test(world);
/** The job of k-ci, the check a CI job runs, in a world of fix-judge (rig-world.json's entry of
 * that world's name): failed at #702's head, the head a fix of k-ci started at; undefined for any
 * other world. */
const fixJob = (world: string) =>
  world.startsWith("fix-judge-")
    ? Object.entries(RIG_WORLD.ciJobs).find(([name]) => name === world)?.[1]
    : undefined;
/** The run of k1 in the worlds of fix-judge, whose change adds greet() and its CLI under
 * packages/greet: the check that no CI job runs, which the implementer's pass at #702's head
 * proves. Its words are plain words of the pull-request line, as K1_RUN's are. */
const FIX_K1_RUN = "bun packages/greet/greet.ts Ada";
/** k1's negative control in the worlds of fix-judge. */
const FIX_K1_CONTROL = "Make greet() print another greeting and the run prints it.";
/** The files the change of a world of fix-judge holds over the base. */
const FIX_FILES = ["packages/greet/greet.ts", "packages/greet/greet.test.ts"];
/** The title of a world of fix-judge's issue. */
const FIX_TITLE = "greet(): a greeting helper for the widgets CLI";
/** The run of k1 with no `ci_check`, which every recorded result names: it proves R1, the line of
 * docs/usage.md that names `widgets --help` and says it lists every command (the `.` stands for the
 * backquote the page writes). A check that served no requirement, or whose run matched any line
 * naming the CLI, gave each architect a reason of its own to send the issue back, so its runs
 * measured that and not the check's rule.
 *
 * The run and each reading below hold only the words the pull-request line Dispatch prints keeps
 * (plainWord, packages/envoy/internal/dispatch/api/results.go: letters, digits and .,;:!?'"-()/).
 * The line leaves every other word out, so a reading that quoted `widgets --help.+every command`
 * printed as `grep -cE 'widgets command'`, a command that prints 0 at the head, and a reviewer of
 * the body that holds that line requested changes on it. */
export const K1_RUN = "grep -cE 'widgets --help. to list every command' docs/usage.md";
const HandoffRef = z.object({ id: z.string(), version: z.number().int() });
/** What `heads-issue` writes, and `heads-events` and rig.sh's heads scenarios read: the run's issue,
 * its title and its Verification document, and once `heads-events` recorded them, its handoff
 * records and, by pull request number, the line of k1 that pull request's body carries: the line
 * Dispatch answered for the newest pass recorded at its head, or for the tester's fail at #703 in
 * sibling-fails. */
const HeadsFixture = z.object({
  issue: z.string(),
  title: z.string(),
  verification: z.string(),
  implement: HandoffRef.optional(),
  test: HandoffRef.optional(),
  review: HandoffRef.optional(),
  lines: z.record(z.string(), z.string()).optional(),
});
/** The tester's and the reviewer's sessions, which write their results and records beside
 * IMPLEMENTER's: never the run's own. */
const TESTER = { kind: "session", id: "rig-tester" } as const;
const REVIEWER = { kind: "session", id: "rig-reviewer" } as const;

/** The world `<scenario> [<case>]` names when heads-scenarios.txt lists it, else undefined: a world
 * of reviewer-own-head is named by its case alone, any other by its scenario's name with the case
 * after it, and a scenario's own world, which names no case, by the scenario's name. */
export function headsWorld(scenario: string, which: string | undefined): string | undefined {
  if (which === "" || !headsCases(scenario).includes(which ?? "")) return undefined;
  if (which === undefined) return scenario;
  return scenario === "reviewer-own-head" ? which : `${scenario}-${which}`;
}

/** `PUT /__fixture/repos` of the GitHub stand-in FAKE_GITHUB_PORT names, which replaces every
 * repository it holds: the run's repository as `seedRepositories` of
 * scripts/e2e/verification-results.ts gives it (its permissions, its installation and its default
 * branch), with `fields` over them. */
async function seedGithub(fields: Record<string, unknown>): Promise<void> {
  const port = harnessPorts().fakeGithub.port;
  if (port === 0) throw new Error("FAKE_GITHUB_PORT names no GitHub stand-in (rig.sh own_github)");
  const response = await fetch(new URL("/__fixture/repos", `http://127.0.0.1:${port}`), {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      [HEADS_REPO]: {
        actions: "read",
        contents: "read",
        pull_requests: "read",
        installation_id: 256,
        default_branch: HEADS_BASE,
        ...fields,
      },
    }),
  });
  if (!response.ok)
    throw new Error(`PUT /__fixture/repos failed: ${response.status} ${await response.text()}`);
}

/** `PUT route` as the agent bearer and the session IMPLEMENTER, with `input` as its JSON body; a
 * refusal names the route and Dispatch's answer. */
async function agentPut(route: string, input: Record<string, unknown>): Promise<void> {
  const response = await fetch(new URL(route, baseUrl), {
    method: "PUT",
    headers: {
      Authorization: `Bearer ${e2eAgentToken}`,
      "Content-Type": "application/json",
    },
    body: JSON.stringify({ ...input, actor: IMPLEMENTER }),
  });
  if (!response.ok)
    throw new Error(`PUT ${route} failed: ${response.status} ${await response.text()}`);
}

/** `heads-issue <file> <scenario> [<case>]`: the run's issue, before rig.sh's heads_fixture makes
 * branches named after it. First the GitHub stand-in holds the run's repository and no pull
 * request, since the delivery settings read GitHub before Dispatch stores them
 * (settings_delivery.go); then the run's project LWEVAL takes the repository as its default and the
 * delivery settings are written, as scripts/e2e/verification-results.ts writes them, since the
 * intake takes an event only while a settings row exists; then the issue, its spec, and its
 * Verification document, whose one check k1 carries `ci_check` in the case ci-only and in
 * ci-pass-and-fail. The fixture names that document, whose block k1 rig.sh's ci_job_results reads.
 * A world of fix-judge has an issue of its own: greet() and its CLI under packages/greet, whose
 * one requirement k1 proves by a run and k-ci by its CI job (fixJob), the check a fix runs for. */
export async function headsIssue(file: string, world: string): Promise<void> {
  await seedGithub({});
  await agentPut("/api/v1/projects/LWEVAL/default-repository", { repo: HEADS_REPO });
  await agentPut("/api/v1/settings/delivery", {
    deploy_repo: HEADS_REPO,
    deploy_workflow_path: ".github/workflows/deploy.yml",
    excluded_repos: [],
    population_authors: ["octocat"],
    pr_checks_workflow_path: ".github/workflows/pr-checks.yml",
    production_job_names: ["deploy / deploy"],
  });
  const fix = fixJob(world);
  const title = fix === undefined ? "A usage page for the widgets CLI" : FIX_TITLE;
  const requirement =
    fix === undefined
      ? "**R1.** `docs/usage.md` names `widgets --help`, which lists every command. From: the issue."
      : "**R1.** `bun packages/greet/greet.ts <name>` prints `Hello, <name>!`, the name trimmed of the spaces at either end. From: the issue.";
  const issue = await createIssue(
    {
      project: "LWEVAL",
      force: true,
      title,
      spec: [
        "## Goal",
        "",
        fix === undefined
          ? "`docs/usage.md` tells a reader how to list the widgets CLI's commands."
          : "The widgets CLI greets a person by name.",
        "",
        "## Requirements",
        "",
        ":::requirement{#r1}",
        requirement,
        ":::",
      ].join("\n"),
    },
    { as: "agent" }
  );
  const ciJob =
    world === "ci-only"
      ? CI_JOB.name
      : world.startsWith("ci-pass-and-fail-")
        ? CI_RUNNING_JOB.name
        : "";
  // k1 serves R1, and its words are R1's, so an architect finds no coverage gap to send back for.
  const serves = `serves="dispatch://${issue.key}/spec#b-r1"`;
  const attributes =
    ciJob !== ""
      ? `${serves} ci_check="${ciJob}" when="before_merge" negative_control="Drop the line from docs/usage.md and the ${ciJob} job fails."`
      : fix !== undefined
        ? `${serves} run="${FIX_K1_RUN}" when="before_merge" negative_control="${FIX_K1_CONTROL}"`
        : `${serves} run="${K1_RUN}" when="before_merge" negative_control="Drop the line from docs/usage.md and grep prints 0 and exits 1."`;
  // In the worlds of a failed CI check the document holds k2 beside k1: a check of R1 that a CI
  // job runs, the test of the page against the commands the CLI lists. In a world of fix-judge it
  // holds k-ci, the check its CI job runs, whose failure at #702's head started the fix.
  const second = ciCheckWorld(world)
    ? [
        "",
        `:::check{#k2 surface="cli" ${serves} ci_check="${CI_FAIL_JOB.name}" when="before_merge" negative_control="Drop a command from the list docs/usage.md gives and the ${CI_FAIL_JOB.name} job fails."}`,
        "The page `docs/usage.md` lists each command that `widgets --help` lists.",
        ":::",
      ]
    : fix !== undefined
      ? [
          "",
          `:::check{#k-ci surface="cli" ${serves} ci_check="${fix.name}" when="before_merge" negative_control="Break what the ${fix.name} job checks and the ${fix.name} job fails."}`,
          `The ${fix.name} job of the repository's CI passes at the head.`,
          ":::",
        ]
      : [];
  const verification = await createIssueArtifact(
    issue.key,
    {
      name: "verification.md",
      content: [
        "# Verification",
        "",
        `:::check{#k1 surface="cli" ${attributes}}`,
        fix === undefined
          ? "`docs/usage.md` names `widgets --help`, which lists every command."
          : "`bun packages/greet/greet.ts <name>` prints `Hello, <name>!`, the name trimmed of the spaces at either end.",
        ":::",
        ...second,
      ].join("\n"),
    },
    { as: "agent" }
  );
  const fixture: z.infer<typeof HeadsFixture> = {
    issue: issue.key,
    title,
    verification: verification.artifact.id,
  };
  writeFileSync(file, `${JSON.stringify(fixture)}\n`);
  // The log is the run's fixture.log, which its agent can read: it names no scenario and no case.
  console.log(`seeded ${issue.key}`);
}

/** One result of k1 that `heads-events` records, at the head of the pull request `pr`. */
interface HeadsResult {
  readonly pr: string;
  readonly role: "implementer" | "tester";
  readonly verdict: "pass" | "fail";
  readonly observed: string;
}
/** The pass of k1 at #702's head that a case records, as the implementer, or as the tester. */
const IMPLEMENTER_PASS: HeadsResult = {
  pr: "702",
  role: "implementer",
  verdict: "pass",
  observed: `${K1_RUN} printed 1: docs/usage.md names widgets --help, which lists every command.`,
};
const TESTER_PASS: HeadsResult = { ...IMPLEMENTER_PASS, role: "tester" };

/** `heads-events <file> <heads file> <scenario> [<case>]`, after heads_fixture: the GitHub
 * stand-in, seeded again since a seed replaces what it holds, now serves the repository's commits
 * and the open pull requests #702 and #703 (#702 alone in architect-pass-and-fail, its cases but
 * ci-check-fail-other-head, and ci-pass-and-fail), each at its head from <heads file>, with a title
 * that names the issue; in the case ci-only a `success` of k1's job at #702's head, in
 * ci-pass-and-fail that job's run there `in_progress`, and in ci-check-fail k2's job `failure` at
 * #702's head, or in ci-check-fail-other-head `success` there and `failure` at #703's. It serves
 * no `commit_pulls`, so an event is the one thing that teaches
 * Dispatch a head. One `opened` envelope of each pull request goes out on the NATS
 * DISPATCH_E2E_NATS_URLS names, with the subject and the body of `publishPullRequest`
 * (scripts/e2e/verification-results.ts). Then the case's results, each as the role it names, and
 * the handoff records, in this order:
 *   - sibling-fails: a pass of the implementer and one of the tester at #702's head, a fail of the
 *     tester at #703's head, then the `test` record;
 *   - ci-only: no result, since the check run is the only evidence, then the `test` record;
 *   - no-implementer: a pass of the tester alone at #702's head, then the `test` record;
 *   - no-tester: a pass of the implementer alone at #702's head, then the `test` record;
 *   - no-verdict-pass: a pass of the implementer and one of the tester at #702's head, then the
 *     `test` record, and nothing at #703's;
 *   - no-verdict-fail: a pass of the implementer at #702's head, the `test` record, then a fail of
 *     the tester there that repeats k1's negative control, so the record lists no failure and
 *     #702's line in `dispatch results` is the one place the fail shows;
 *   - reviewer-pass-and-fail: at #702's head a pass of the implementer, a pass of the tester, the
 *     `test` record, then a fail of the tester, the order a tester whose later run failed leaves;
 *   - architect-pass-and-fail: the same, then the `review` record; its cases no-verdict-pass and
 *     no-verdict-fail: the results and the record of the reviewer-own-head case of that name at
 *     #702's head, then the `review` record;
 *   - ci-pass-and-fail: no result of a role, since k1 is CI's; the `implement` record for the
 *     tester, the `test` record for the reviewer, and the `test` and `review` records for the
 *     architect. The CI job's own pass and fail at #702's head are rig.sh's (ci_job_results), since
 *     only a job's results token records them through the route.
 *   - ci-check-fail and ci-check-fail-other-head, of reviewer-own-head and architect-pass-and-fail:
 *     a pass of the implementer and one of the tester of k1 at the head of each open pull request,
 *     then the `test` record, and for the architect the `review` record. k2, the check a CI job
 *     runs, holds no result of a role: its state is its job's.
 *   - fix-judge's worlds: #702 alone, on the issue's own branch legion/<issue>, at whose head
 *     k-ci's job failed (its first attempt); a pass of the implementer of k1 there and the
 *     `implement` record, then, but in tester-beside, whose tester is the one that runs, a pass of
 *     the tester and the `test` record.
 * Each record is written as its role's session (IMPLEMENTER, TESTER, REVIEWER), with the fields its
 * role's prompt names (roles/tester.md, roles/reviewer.md) and no other: the tester's holds no
 * `implementerProof`, `failures` or `failed`, since each failure is a fail result on its check and
 * no phase completion or record carries a verdict. It adds each record's `{id, version}` and each
 * pull request body's line of k1 to <file>. */
export async function headsEvents(file: string, headsFile: string, world: string): Promise<void> {
  const fixture = HeadsFixture.parse(JSON.parse(readFileSync(file, "utf8")));
  const heads = z
    .record(z.string(), z.string().regex(/^[0-9a-f]{40}$/))
    .parse(JSON.parse(readFileSync(headsFile, "utf8")));
  const ciRunning = world.startsWith("ci-pass-and-fail-");
  const ciCheck = ciCheckWorld(world);
  const otherHead = ciCheck && world.endsWith("-other-head");
  const fix = fixJob(world);
  const prs =
    (world.startsWith("architect-pass-and-fail") && !otherHead) || ciRunning || fix !== undefined
      ? ["702"]
      : ["702", "703"];
  const headOf = (pr: string): string => {
    const head = heads[pr];
    if (head === undefined) throw new Error(`${headsFile} names no head of #${pr}`);
    return head;
  };
  const title = `${fixture.title} (${fixture.issue})`;
  await seedGithub({
    commits: prs.map(headOf),
    pulls: Object.fromEntries(
      prs.map((pr) => [pr, pullSeed(fixture, pr, headOf(pr), fix !== undefined)])
    ),
    ...(world === "ci-only"
      ? {
          check_runs: {
            [headOf("702")]: [
              {
                id: CI_JOB.checkRun,
                name: CI_JOB.name,
                check_suite_id: CI_JOB.suite,
                status: "completed",
                conclusion: "success",
                completed_at: new Date(Date.now() - 60_000).toISOString(),
                html_url: `https://github.com/${HEADS_REPO}/actions/runs/${CI_JOB.run}/job/${CI_JOB.checkRun}`,
                output: {
                  title: `${CI_JOB.name} success`,
                  summary: `The ${CI_JOB.name} job concluded success.`,
                },
              },
            ],
          },
        }
      : ciRunning
        ? {
            check_runs: {
              [headOf("702")]: [
                {
                  id: CI_RUNNING_JOB.checkRun,
                  name: CI_RUNNING_JOB.name,
                  check_suite_id: CI_RUNNING_JOB.suite,
                  status: "in_progress",
                  conclusion: null,
                  completed_at: null,
                  html_url: `https://github.com/${HEADS_REPO}/actions/runs/${CI_RUNNING_JOB.run}/job/${CI_RUNNING_JOB.checkRun}`,
                  output: {
                    title: `${CI_RUNNING_JOB.name} in progress`,
                    summary: `The ${CI_RUNNING_JOB.name} job is running its third attempt.`,
                  },
                },
              ],
            },
          }
        : ciCheck
          ? {
              // k2's job at each head the world runs it at: failed at #702's head, or passed there
              // and failed at #703's.
              check_runs: Object.fromEntries(
                (otherHead
                  ? ([
                      ["702", CI_PASS_JOB],
                      ["703", CI_FAIL_JOB],
                    ] as const)
                  : ([["702", CI_FAIL_JOB]] as const)
                ).map(([pr, job]) => [
                  headOf(pr),
                  [
                    {
                      id: job.checkRun,
                      name: job.name,
                      check_suite_id: job.suite,
                      status: "completed",
                      conclusion: job.conclusion ?? "success",
                      completed_at: new Date(Date.now() - 60_000).toISOString(),
                      html_url: `https://github.com/${HEADS_REPO}/actions/runs/${job.run}/job/${job.checkRun}`,
                      output: {
                        title: `${job.name} ${job.conclusion ?? "success"}`,
                        summary: `The ${job.name} job concluded ${job.conclusion ?? "success"}.`,
                      },
                    },
                  ],
                ])
              ),
            }
          : {}),
    // k-ci's job in a world of fix-judge: its first attempt, failed at #702's head.
    ...(fix === undefined ? {} : { check_runs: { [headOf("702")]: [fixCheckRun(fix, false)] } }),
  });
  await publishEnvelopes(
    prs.map((pr) => ({
      pr,
      event: `skill-scenarios-pr-${pr}-opened-${headOf(pr).slice(0, 7)}`,
      payload: {
        action: "opened",
        base_ref: HEADS_BASE,
        body: `Dispatch: ${fixture.issue}`,
        head_ref: pullSeed(fixture, pr, headOf(pr), fix !== undefined).head_ref,
        head_repo: HEADS_REPO,
        head_sha: headOf(pr),
        kind: "pr",
        merged: "false",
        number: pr,
        repo: HEADS_REPO,
        title,
        updated_at: new Date(Date.now() - 60_000).toISOString(),
        url: `https://github.com/${HEADS_REPO}/pull/${pr}`,
      },
    }))
  );

  const lines: Record<string, string> = {};
  const record = async (result: HeadsResult): Promise<void> => {
    const answered = (await agentPost(
      `/api/v1/issues/${encodeURIComponent(fixture.issue)}/results`,
      {
        check: "k1",
        role: result.role,
        verdict: result.verdict,
        commit: headOf(result.pr),
        surface: "cli",
        run: fix === undefined ? K1_RUN : FIX_K1_RUN,
        observed: result.observed,
        negative_control:
          fix === undefined
            ? "Drop the line from docs/usage.md and grep prints 0 and exits 1."
            : FIX_K1_CONTROL,
        actor: result.role === "implementer" ? IMPLEMENTER : TESTER,
      }
    )) as { line?: unknown };
    if (typeof answered.line !== "string")
      throw new Error(`the record of ${result.role}'s ${result.verdict} answered no line`);
    // A pull request's body names the newest pass at its head, or else the newest result there.
    if (result.verdict === "pass" || lines[result.pr] === undefined)
      lines[result.pr] = answered.line;
    console.log(`recorded ${result.role}'s ${result.verdict} of k1 at #${result.pr}'s head`);
  };
  const head = headOf("702");
  const handoff = async (phase: "implement" | "test" | "review") => {
    const body =
      phase === "implement"
        ? {
            schemaVersion: 1,
            phase,
            issue: fixture.issue,
            head,
            filesChanged: fix === undefined ? ["docs/usage.md"] : FIX_FILES,
          }
        : phase === "test"
          ? {
              schemaVersion: 1,
              phase,
              issue: fixture.issue,
              head,
              classSearch: [],
            }
          : {
              schemaVersion: 1,
              phase,
              issue: fixture.issue,
              head,
              decision: "approved",
              critical: 0,
              important: 0,
              minor: 0,
              keyFindings: [],
            };
    const recorded = await createHandoff(
      fixture.issue,
      phase,
      body,
      phase === "implement" ? IMPLEMENTER : phase === "test" ? TESTER : REVIEWER
    );
    console.log(`recorded the ${phase} handoff, version ${recorded.version} (${recorded.id})`);
    return { id: recorded.id, version: recorded.version };
  };
  // The tester's later fail. In architect-pass-and-fail it reads as noise of the runner, not as a
  // defect of the change: under the old rule (a fail after a pass leaves that pass standing until
  // someone retracts it) an architect then has a reason to sign off on the tester's pass and the
  // reviewer's approval, which only the rule "a pass and a fail at one head leave it not known,
  // whoever recorded them" removes. A fail that repeats k1's negative control is a defect that any
  // reader sends back, with or without the rule, so it could not show the rule.
  const laterFail: HeadsResult = {
    ...TESTER_PASS,
    verdict: "fail",
    observed:
      world === "architect-pass-and-fail"
        ? "A second run stopped before grep ran: the runner lost its checkout, so the run printed nothing and exited 1."
        : `A second run of ${K1_RUN} printed 0 and exited 1.`,
  };
  let implement: z.infer<typeof HandoffRef> | undefined;
  let test: z.infer<typeof HandoffRef> | undefined;
  let review: z.infer<typeof HandoffRef> | undefined;
  if (world === "sibling-fails") {
    await record(IMPLEMENTER_PASS);
    await record(TESTER_PASS);
    await record({
      pr: "703",
      role: "tester",
      verdict: "fail",
      observed: "grep: docs/usage.md: No such file or directory, so grep -c exited 2.",
    });
    test = await handoff("test");
  } else if (world === "ci-only") {
    test = await handoff("test");
  } else if (world === "no-implementer") {
    await record(TESTER_PASS);
    test = await handoff("test");
  } else if (world === "no-tester") {
    await record(IMPLEMENTER_PASS);
    test = await handoff("test");
  } else if (world === "no-verdict-pass" || world === "architect-pass-and-fail-no-verdict-pass") {
    await record(IMPLEMENTER_PASS);
    await record(TESTER_PASS);
    test = await handoff("test");
    if (world === "architect-pass-and-fail-no-verdict-pass") review = await handoff("review");
  } else if (world === "no-verdict-fail" || world === "architect-pass-and-fail-no-verdict-fail") {
    await record(IMPLEMENTER_PASS);
    test = await handoff("test");
    await record(laterFail);
    if (world === "architect-pass-and-fail-no-verdict-fail") review = await handoff("review");
  } else if (world === "ci-pass-and-fail-tester") {
    implement = await handoff("implement");
  } else if (ciRunning) {
    test = await handoff("test");
    if (world === "ci-pass-and-fail-architect") review = await handoff("review");
  } else if (ciCheck) {
    // k1, the check that no CI job runs, passes from both roles at each open head the world has;
    // k2's state is its job's, at the heads seeded above.
    for (const pr of prs) {
      await record({ ...IMPLEMENTER_PASS, pr });
      await record({ ...TESTER_PASS, pr });
    }
    test = await handoff("test");
    if (world.startsWith("architect-pass-and-fail")) review = await handoff("review");
  } else if (world === "reviewer-pass-and-fail" || world === "architect-pass-and-fail") {
    await record(IMPLEMENTER_PASS);
    await record(TESTER_PASS);
    test = await handoff("test");
    await record(laterFail);
    if (world === "architect-pass-and-fail") review = await handoff("review");
  } else if (fix !== undefined) {
    // The fix's worlds: the implementer's pass of k1, its record, and where the tester's step has
    // passed before the fix started (each world but the tester's own), the tester's pass and record.
    const observed = `${FIX_K1_RUN} printed Hello, Ada! and exited 0.`;
    await record({ ...IMPLEMENTER_PASS, observed });
    implement = await handoff("implement");
    if (world !== "fix-judge-tester-beside") {
      await record({ ...TESTER_PASS, observed });
      test = await handoff("test");
    }
  } else {
    // A world the catalog lists and this function records nothing for would leave its run's issue
    // without the results its agent is to judge.
    throw new Error(`heads-events records no results for the world ${world}`);
  }
  const written: z.infer<typeof HeadsFixture> = {
    ...fixture,
    ...(implement === undefined ? {} : { implement }),
    ...(test === undefined ? {} : { test }),
    ...(review === undefined ? {} : { review }),
    lines,
  };
  writeFileSync(file, `${JSON.stringify(written)}\n`);
}

/** The pull request `pr` of the run's issue as the GitHub stand-in serves it, at `head`: on the
 * branch legion/<issue>-<pr>, or in a world of fix-judge (`fix`) on the issue's own branch
 * legion/<issue>, the branch of the task a fix pushes once at its end. */
function pullSeed(fixture: z.infer<typeof HeadsFixture>, pr: string, head: string, fix: boolean) {
  return {
    head_sha: head,
    state: "open",
    merged: false,
    title: `${fixture.title} (${fixture.issue})`,
    body: `Dispatch: ${fixture.issue}`,
    head_ref: fix ? `legion/${fixture.issue}` : `legion/${fixture.issue}-${pr}`,
    head_repo: HEADS_REPO,
    base_ref: HEADS_BASE,
  };
}

/** k-ci's job in a world of fix-judge as the GitHub stand-in serves its check run: its first
 * attempt, which failed, or (`settled`) the second attempt a re-run made, as it concluded. */
function fixCheckRun(job: RigCIJob, settled: boolean) {
  const id = settled ? (job.rerun?.checkRun ?? job.checkRun) : job.checkRun;
  const conclusion = settled ? (job.rerun?.conclusion ?? "failure") : (job.conclusion ?? "success");
  return {
    id,
    name: job.name,
    check_suite_id: job.suite,
    status: "completed",
    conclusion,
    completed_at: new Date(Date.now() - 60_000).toISOString(),
    html_url: `https://github.com/${HEADS_REPO}/actions/runs/${job.run}/job/${id}`,
    output: {
      title: `${job.name} ${conclusion}`,
      summary: `The ${job.name} job concluded ${conclusion}.`,
    },
  };
}

/** Publishes one envelope of the GitHub listener for each of `envelopes` on the NATS
 * DISPATCH_E2E_NATS_URLS names, on the subject of its pull request, with the body of
 * `publishPullRequest` (scripts/e2e/verification-results.ts): `payload` is the listener's own,
 * `event` its id. */
async function publishEnvelopes(
  envelopes: { pr: string; event: string; payload: Record<string, unknown> }[]
): Promise<void> {
  const natsServers = (process.env.DISPATCH_E2E_NATS_URLS ?? "")
    .split(",")
    .map((server) => server.trim())
    .filter((server) => server !== "");
  if (natsServers.length === 0)
    throw new Error("DISPATCH_E2E_NATS_URLS names no NATS (rig.sh own_github)");
  const nats = await connect({ name: "skill-scenarios-seed", servers: natsServers });
  const codec = StringCodec();
  const [owner, name] = HEADS_REPO.split("/");
  for (const { pr, event, payload } of envelopes) {
    const subject = `notifications.github.${String(owner).replaceAll(".", "_")}.${String(name).replaceAll(".", "_")}.pr.${pr}`;
    nats.publish(
      subject,
      codec.encode(
        JSON.stringify({
          dedupe_key: event,
          event_id: event,
          issued_at: Date.now(),
          payload: JSON.stringify(payload),
          payload_summary: subject,
          source: "github",
          source_event_id: event,
          topic: subject,
          trace_id: event,
        })
      )
    );
    console.log(`published the ${String(payload.kind)} envelope of #${pr} on ${subject}`);
  }
  await nats.flush();
  await nats.close();
}

/** `heads-settle <file> <heads file> fix-judge <case>`, once the agent's turn that ran k-ci's job
 * once more has ended: the GitHub stand-in, seeded again whole as heads-events seeded it, now
 * serves that job's second attempt at #702's head as it concluded (rig-world.json's `rerun`), and
 * the settlement of #702's checks (`kind: checks`, which the listener publishes once every check
 * run of a head has ended) goes out on the NATS, so the run's Dispatch reads the check runs at the
 * head again and publishes k-ci's new state. A world with no job that runs once more is refused. */
export async function headsSettle(file: string, headsFile: string, world: string): Promise<void> {
  const fixture = HeadsFixture.parse(JSON.parse(readFileSync(file, "utf8")));
  const head = z
    .record(z.string(), z.string().regex(/^[0-9a-f]{40}$/))
    .parse(JSON.parse(readFileSync(headsFile, "utf8")))["702"];
  const fix = fixJob(world);
  if (fix?.rerun === undefined) throw new Error(`the world ${world} runs no job once more`);
  if (head === undefined) throw new Error(`${headsFile} names no head of #702`);
  await seedGithub({
    commits: [head],
    pulls: { "702": pullSeed(fixture, "702", head, true) },
    check_runs: { [head]: [fixCheckRun(fix, true)] },
  });
  await publishEnvelopes([
    {
      pr: "702",
      event: `skill-scenarios-pr-702-checks-${head.slice(0, 7)}-2`,
      payload: { kind: "checks", repo: HEADS_REPO, number: "702", sha: head },
    },
  ]);
}
