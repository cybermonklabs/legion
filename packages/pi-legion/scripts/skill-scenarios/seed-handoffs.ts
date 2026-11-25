// The phase seeds of a worker run's own scratch Dispatch, and the read-back of what the run left
// there: the records each phase before the one under test wrote, the passing results that go
// with them, the statuses the daemon wrote as each phase ended, and the pull request Dispatch
// holds for the issue. seed.ts owns the command line and every other mode; these modes are here
// because they are one world — one fixture file, one issue, one check — and seed.ts had grown
// past a thousand lines around them.
//
//   seed.ts plan <file> <implement body file> <base commit> <remote>   planSeed
//   seed.ts pull-request <file> <gh answers file>             pullRequestHead
//   seed.ts implement-handoff <file> <body file> <commit>     phaseHandoff, phase implement
//   seed.ts tester-result <file> <body file> <commit>         phaseHandoff, phase test
//   seed.ts review-handoff <file> <body file>                 phaseHandoff, phase review
//   seed.ts capture-handoffs <file> <run dir>                 captureHandoffs
//   seed.ts capture-results <file> <run dir>                  captureResults
//
// seed.ts's own header says what each writes and reads.
import { readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import {
  type Actor,
  type Handoff,
  type HandoffSummary,
  type IssueCheckResults,
  roleToken,
} from "@legion/contracts";
import { z } from "zod";
import {
  baseUrl,
  createHandoff,
  createIssueArtifact,
  e2eAgentToken,
  patchIssue,
  putStep,
} from "../../../dispatch/e2e/api";
import { labelCheckout, labelShapes } from "./daemon-pane";
import { WORLD_PROJECT } from "./run-names";
import { GENERATION } from "./standin-record";
import { runsStep, STEPS, stepEnd, stepRun, stepStart } from "./step-records";
import { type CheckCovers, WORLD_REPO, worldPlan } from "./world-issue";

/** One record a seed wrote on the run's issue. */
const SeededRecord = z.object({ id: z.string(), version: z.number().int() });
/** What `tester-proof` writes, and every mode of this file reads: the run's issue, what its check
 * covers (world-issue.ts's CheckCovers, `r2` where the fixture predates the field), and each phase
 * record a seed has written on it. */
export const TesterFixture = z.object({
  issue: z.string(),
  covers: z.enum(["r2", "no-name"]).default("r2"),
  plan: SeededRecord.optional(),
  implement: SeededRecord.optional(),
  test: SeededRecord.optional(),
  review: SeededRecord.optional(),
});
type TesterFixture = z.infer<typeof TesterFixture>;
/** The sessions the worker's predecessors wrote their handoffs as: the planner's, the
 * implementer's, and the tester's and the reviewer's where a scenario seeds one, never the run's
 * own. Each id has the shape of any role's session id (a UUIDv7, as Envoy registers one), since
 * the issue's events show it to every agent that reads them: `rig-<role>` was the source of 68 of
 * the 107 sentences of the batch at c273084d that called the world a test. */
export const PLANNER = { kind: "session", id: "01a1b3a8-5d17-7e40-b3c9-8f2a6d0e4b15" } as const;
export const IMPLEMENTER = { kind: "session", id: "01a1b3f2-6c4e-7a18-9d2b-5e8f0c71a4d3" } as const;
export const TESTER = { kind: "session", id: "01a1b402-1f7d-7c55-8a3e-2b6d9f04c812" } as const;
export const REVIEWER = { kind: "session", id: "01a1b41e-93a0-7e2c-b647-0d5c8e2f1a96" } as const;
/** The session production's Legion daemon writes every status of an issue of WORLD_PROJECT as,
 * `legion-daemon:<PROJECT>` (packages/daemon/internal/dispatch/client.go, DaemonSession). */
export const DAEMON = { kind: "session", id: `legion-daemon:${WORLD_PROJECT}` } as const;

/** Writes `status` on the run's issue as the daemon writes it when the phase before it ends
 * (packages/daemon/internal/workflow/table.go, PhaseStatus), with `labels_add` when the write is
 * the daemon's hand-over, which adds the `legion` label in the same write (client.go, HandOver). */
export function daemonStatus(issue: string, status: string, labelsAdd?: string[]) {
  return patchIssue(
    issue,
    labelsAdd === undefined ? { status } : { status, labels_add: labelsAdd },
    { as: "agent", actor: DAEMON }
  );
}

/** Each phase a seed writes a handoff for: the predecessor's session, which records it; the
 * role whose passing result on check `k1` goes with it, for the two phases whose role records
 * one — a review round records none; and the status the daemon writes as that phase ends, for the
 * two whose end moves it: `testing` after implementing, `needs_review` after testing, which a
 * review round's end keeps, since reviewing and accepting both show it (PhaseStatus). The phase is
 * the key, so a caller names it once and the four cannot disagree: the session, the role and the
 * phase were three arguments, and a handoff written under the tester's session as the
 * implementer's phase is a world no run of the rig has. */
const PHASE_SEED = {
  implement: { actor: IMPLEMENTER as Actor, role: "implementer", status: "testing" },
  test: { actor: TESTER as Actor, role: "tester", status: "needs_review" },
  review: { actor: REVIEWER as Actor, role: undefined, status: undefined },
} as const;
type HandoffPhase = keyof typeof PHASE_SEED;

/** The negative control each role recorded beside its pass, as the check it ran names it: both
 * halves of R2 where the check covers R2, and the no-name half alone where the check is the one
 * that missed the blank name. */
const NEGATIVE_CONTROL: Readonly<Record<CheckCovers, string>> = {
  r2: "bun greet.ts → exit 2, usage: greet.ts <name> on stderr; bun greet.ts '   ' → exit 2, the same line on stderr",
  "no-name": "bun greet.ts → exit 2, usage: greet.ts <name> on stderr",
};

/** The fixture `file` names. */
function loadFixture(file: string): TesterFixture {
  return TesterFixture.parse(JSON.parse(readFileSync(file, "utf8")));
}

/** One predecessor's handoff on the run's issue, as `dispatch handoff-write` records it under
 * that predecessor's own session, and, for a phase whose role records one, its passing result
 * against the issue's check `k1` at `commit`, as `dispatch result` records it, then the status the
 * daemon writes as that phase ends; the record goes into the fixture under its phase. A body that
 * is no JSON object is the fixture's fault, refused before anything is written. It is the whole of
 * `implement-handoff`, `tester-result` and `review-handoff`, which are one seeding under three
 * phase words: seed.ts's header says what each of the three writes. */
export async function phaseHandoff(
  file: string,
  bodyFile: string,
  phase: HandoffPhase,
  commit = ""
): Promise<void> {
  const { actor, role, status } = PHASE_SEED[phase];
  const fixture = loadFixture(file);
  const body: unknown = JSON.parse(readFileSync(bodyFile, "utf8"));
  if (typeof body !== "object" || body === null || Array.isArray(body))
    throw new Error(`${bodyFile} holds no JSON object, which a handoff body is`);
  const recorded = await createHandoff(fixture.issue, phase, body, actor);
  if (role !== undefined)
    await agentPost(`/api/v1/issues/${encodeURIComponent(fixture.issue)}/results`, {
      check: "k1",
      role,
      verdict: "pass",
      commit,
      surface: "cli",
      run: "bun greet.ts Ada",
      observed: "prints Hello, Ada! and exits 0",
      negative_control: NEGATIVE_CONTROL[fixture.covers],
      actor,
    });
  if (status !== undefined) await daemonStatus(fixture.issue, status);
  const record = { id: recorded.id, version: recorded.version };
  writeFileSync(file, `${JSON.stringify({ ...fixture, [phase]: record })}\n`);
  console.log(
    `seeded ${fixture.issue}'s ${phase} handoff, version ${recorded.version} (${recorded.id})`
  );
}

/** The implement handoff's one field the plan reads: `head`, the commit the implementer pushed. A
 * handoff holds its head and, where its role has one, a short note, and nothing else (#5287). */
const ImplementBody = z.object({ head: z.string().regex(/^[0-9a-f]{40}$/) });

/** `plan <file> <implement body file> <base commit> <remote>`: what the planner left before the
 * implementer began. The issue's plan.md (world-issue.ts's worldPlan, a task for each file the
 * implementer's change touches beyond the CLI's own: the files `git diff` names from `base` to the
 * implement body's `head` on `remote`, the run's own remote, since what the implementer changed is
 * the diff and no field of its handoff), which makes it the issue's plan document; the plan
 * review's approval of its version 1, which `dispatch plan-approve` records and the `Approval:`
 * line of `dispatch read --artifact plan-md` names, so every later role builds from that version;
 * and the `plan` handoff, `head` alone, the head the workspace stood on, `base`, the world's
 * default branch before the implementer's commit. All three under the planner's session. A base
 * that is no full commit, and a remote that holds no diff from it to the head, are the caller's
 * fault, refused before anything is written. */
export async function planSeed(
  file: string,
  implementFile: string,
  base: string,
  remote: string
): Promise<void> {
  if (!/^[0-9a-f]{40}$/.test(base)) throw new Error(`${base} is no 40-character commit`);
  const fixture = loadFixture(file);
  const { head } = ImplementBody.parse(JSON.parse(readFileSync(implementFile, "utf8")));
  const diff = Bun.spawnSync(["git", "-C", remote, "diff", "--name-only", "-z", base, head]);
  if (diff.exitCode !== 0)
    throw new Error(
      `${remote} holds no diff from ${base} to ${head}: ${diff.stderr.toString().trim()}`
    );
  const files = diff.stdout.toString().split("\0").filter(Boolean);
  const options = { as: "agent", actor: PLANNER } as const;
  const { artifact, version } = await createIssueArtifact(
    fixture.issue,
    { name: "plan.md", content: worldPlan(fixture.issue, files) },
    options
  );
  await agentPost(`/api/v1/artifacts/${encodeURIComponent(artifact.id)}/plan-reviews`, {
    actor: PLANNER,
    version: version.number,
  });
  const recorded = await createHandoff(fixture.issue, "plan", { head: base }, PLANNER);
  const record = { id: recorded.id, version: recorded.version };
  writeFileSync(file, `${JSON.stringify({ ...fixture, plan: record })}\n`);
  console.log(`seeded ${fixture.issue}'s plan.md, approved at version 1, and its plan handoff`);
}

/** The pull request the world's gh answers name (rig.sh's fixture_write, its `gh pr view` route),
 * as far as a head row reads it: its head is the run's remote's, which the caller passes. */
const WorldPullRequest = z.object({
  number: z.number().int().positive(),
  url: z.string(),
  title: z.string(),
  state: z.literal("OPEN"),
  headRefName: z.string(),
  baseRefName: z.string(),
});
const GhAnswers = z.object({
  routes: z.array(z.object({ match: z.string(), stdout: z.unknown().optional() })),
});
/** The variable `pull-request` reads the URL of the run's own Dispatch database from, never an
 * argument, which every user of the box reads (rig.sh's seed_ts sets it). */
export const DATABASE_URL_VARIABLE = "SEED_DATABASE_URL";

/** `pull-request <file> <gh answers file> <remote>`: the pull request the world's gh answers name,
 * at the commit its branch is at on `remote`, the run's own remote, stored on the run's own
 * Dispatch as its `pull_request` intake stores the head of every pull request GitHub sends it
 * (packages/envoy/internal/dispatch/delivery/pull_request_heads.go, upsertHeadRow): its
 * repository, number, head commit, head and base branches, state, title and URL, and the issue it
 * is attributed to, by the source that names it, its head branch (`legion/<issue>`,
 * attribution.go's `branch`), read now. That row is what a `before_merge` check's results are
 * compared with (api/results_comparison.go, headBases), so a result recorded at the head the agent
 * reads stands at it and `dispatch results` prints `A pass stands: yes`. A later call after a push
 * moves the row, as the push's own event moves it: rig.sh's run_threads_closed makes one after the
 * implementer's half, so the reviewer reads the approved head's passes one push below the head.
 *
 * The row is written to the database because nothing else of this world can write it: Dispatch
 * learns a head from a `pull_request` envelope, which reaches it over NATS, and from a read of
 * GitHub, and the world's Dispatch has neither (rig.sh's start_dispatch). It is the intake's own
 * statement, a write of no field the intake leaves alone; what it does not write is the
 * `issue.pull_request_attribution_changed` event the intake appends with it.
 *
 * A pull request of another repository than the issue's, one whose branch names another issue,
 * both of which Dispatch attributes to nobody (attributionHeld), and a remote that holds no such
 * branch, are refused before anything is written. */
export async function pullRequestHead(
  file: string,
  answersFile: string,
  remote: string
): Promise<void> {
  const fixture = loadFixture(file);
  const route = GhAnswers.parse(JSON.parse(readFileSync(answersFile, "utf8"))).routes.find(
    (candidate) => candidate.match === "^gh pr view"
  );
  if (route === undefined) throw new Error(`${answersFile} answers no \`gh pr view\``);
  const pr = WorldPullRequest.parse(route.stdout);
  const [, repo, number] =
    /^https:\/\/github\.com\/([^/]+\/[^/]+)\/pull\/(\d+)$/.exec(pr.url) ?? [];
  if (repo !== WORLD_REPO || Number(number) !== pr.number)
    throw new Error(`${pr.url} is no pull request #${pr.number} of ${WORLD_REPO}`);
  if (pr.headRefName !== `legion/${fixture.issue}`)
    throw new Error(`the branch ${pr.headRefName} names no issue but legion/${fixture.issue}`);
  const read = Bun.spawnSync(["git", "-C", remote, "rev-parse", "--verify", pr.headRefName]);
  const head = read.stdout.toString().trim();
  if (read.exitCode !== 0 || !/^[0-9a-f]{40}$/.test(head))
    throw new Error(`${remote} holds no branch ${pr.headRefName}`);
  const url = process.env[DATABASE_URL_VARIABLE];
  if (url === undefined || url === "")
    throw new Error(`${DATABASE_URL_VARIABLE} names no database for the pull request's head`);
  const sql = new Bun.SQL({ url, max: 1 });
  try {
    await sql`
      insert into pull_request_heads (
        repo, number, head_sha, head_ref, base_ref, state, issue_key, attribution_source,
        title, url, checked_at
      )
      values (
        ${repo}, ${pr.number}, ${head}, ${pr.headRefName}, ${pr.baseRefName}, 'open',
        ${fixture.issue}, 'branch', ${pr.title}, ${pr.url}, now()
      )
      on conflict (repo, number) do update set
        head_sha = excluded.head_sha, head_ref = excluded.head_ref, base_ref = excluded.base_ref,
        state = excluded.state, issue_key = excluded.issue_key,
        attribution_source = excluded.attribution_source, title = excluded.title,
        url = excluded.url, checked_at = excluded.checked_at
    `;
  } finally {
    await sql.close();
  }
  console.log(`seeded ${fixture.issue}'s pull request #${pr.number} at ${head}`);
}

/** `GET route` as the agent bearer, its JSON answer; a refusal names the route and Dispatch's answer. */
async function agentGet<T>(route: string): Promise<T> {
  const response = await fetch(new URL(route, baseUrl), {
    headers: { Authorization: `Bearer ${e2eAgentToken}` },
  });
  if (!response.ok)
    throw new Error(`GET ${route} failed: ${response.status} ${await response.text()}`);
  return (await response.json()) as T;
}

/** `POST route` as the agent bearer, with `input` as its JSON body; a refusal names the route and
 * Dispatch's answer. */
async function agentPost(route: string, input: unknown): Promise<void> {
  const response = await fetch(new URL(route, baseUrl), {
    method: "POST",
    headers: {
      Authorization: `Bearer ${e2eAgentToken}`,
      "Content-Type": "application/json",
    },
    body: JSON.stringify(input),
  });
  if (!response.ok)
    throw new Error(`POST ${route} failed: ${response.status} ${await response.text()}`);
}

/** `capture-handoffs <file> <run dir>`: every handoff record of the run's issue, the seeded
 * implement record among them, newest first as `GET …/handoffs` lists them, each read whole by id
 * (`GET /api/v1/handoffs/{id}`) for its body. */
export async function captureHandoffs(file: string, runDir: string): Promise<void> {
  const { issue } = loadFixture(file);
  const summaries = await agentGet<HandoffSummary[]>(
    `/api/v1/issues/${encodeURIComponent(issue)}/handoffs`
  );
  const records: Handoff[] = [];
  for (const summary of summaries)
    records.push(await agentGet<Handoff>(`/api/v1/handoffs/${encodeURIComponent(summary.id)}`));
  writeFileSync(path.join(runDir, "handoffs.json"), `${JSON.stringify(records)}\n`);
  console.log(`captured ${issue}: ${records.length} handoff records`);
}

/** One result of results.json, as `capture-results` writes it and score.ts's `Results` reads it:
 * the check it is of (its block id), the commit it names, the session and the role that recorded
 * it, its verdict, the text it observed (where a result that records an earlier pass as holding
 * gives its reason), and when Dispatch recorded it, which an agent's own run of the check is
 * ordered against. */
interface CapturedResult {
  check: string;
  commit: string;
  session: string;
  role: string;
  verdict: string;
  observed: string;
  at: string;
}

/** `capture-results <file> <run dir>`: every result recorded against a check of the run's issue,
 * the seeded passes among them, in the order `GET /api/v1/issues/{key}/results` answers them (each
 * check of the issue's Verification document in document order, each check's results as Dispatch
 * lists them), as results.json in <run dir>. */
export async function captureResults(file: string, runDir: string): Promise<void> {
  const { issue } = loadFixture(file);
  const read = await agentGet<IssueCheckResults>(
    `/api/v1/issues/${encodeURIComponent(issue)}/results`
  );
  const results: CapturedResult[] = (read.checks ?? []).flatMap((check) =>
    (check.results ?? []).map((result) => ({
      check: result.check_block_id,
      commit: result.commit_sha,
      session: result.session,
      role: result.role,
      verdict: result.verdict,
      observed: result.observed,
      at: result.created_at,
    }))
  );
  writeFileSync(path.join(runDir, "results.json"), `${JSON.stringify(results)}\n`);
  console.log(`captured ${issue}: ${results.length} results`);
}

/** The phases a seed records, in the order the workflow runs them, each with the word its handoff
 * is seeded under. */
const SEEDED_PHASES = [
  ["planning", "plan"],
  ["implementing", "implement"],
  ["testing", "test"],
  ["reviewing", "review"],
] as const;

/** `steps <file> <project> <phase> [model]`: the record of each run of a step the world's history
 * holds, as the label's daemon wrote it as each phase ran (workflow/steps.go's `startStep` and
 * `endStep`, step-records.ts): for each phase whose handoff a seed recorded, in the order the
 * workflow runs them, the start of its one run, at the moment the phase before it ended (the
 * issue's filing for planning, which the seeds hand to Legion and whose spec they approve as they
 * file it), under the claim of its role in the Legion project `project` (`roleToken`), and the end
 * the move out of it wrote: passed, at the moment its handoff was recorded, the report that
 * handoff, the session that wrote it and `model`, the model the label's agents answer with
 * (rig.sh's `label_model`), where it is given. The run of `phase`, the one the issue stands in, is
 * left open: its start alone, whether a seed recorded its handoff (a review round whose approval
 * waits) or not, under its role's claim, the architect's in `accepting`. Each write goes through
 * the run's own Dispatch as the daemon's outbox writes it, under `legion-daemon:<PROJECT>`
 * (dispatch/e2e/api.ts's `putStep`). A label whose daemon writes no step record (daemon-pane.ts's
 * `labelShapes`' `writesSteps`, read off the checkout the run's built-from names) gets none. A
 * phase that runs no step is the caller's fault, refused before anything is written. */
export async function stepsSeed(
  file: string,
  project: string,
  phase: string,
  model = ""
): Promise<void> {
  if (!runsStep(phase)) throw new Error(`${phase} runs no step`);
  const fixture = loadFixture(file);
  if (!labelShapes(labelCheckout(path.dirname(file))).writesSteps) {
    console.log(`seeded no step of ${fixture.issue}: the label's daemon records none`);
    return;
  }
  const filed = await agentGet<{ created_at: string }>(
    `/api/v1/issues/${encodeURIComponent(fixture.issue)}`
  );
  let since = filed.created_at;
  let written = 0;
  for (const [ran, word] of SEEDED_PHASES) {
    const record = fixture[word];
    if (record === undefined) continue;
    const run = stepRun(
      fixture.issue,
      GENERATION,
      ran,
      roleToken(project, fixture.issue, STEPS[ran].role),
      1
    );
    await putStep(fixture.issue, run.id, stepStart(run, since));
    written += 1;
    if (ran === phase) continue;
    const held = await agentGet<Handoff>(`/api/v1/handoffs/${encodeURIComponent(record.id)}`);
    await putStep(
      fixture.issue,
      run.id,
      stepEnd(run, {
        startedAt: since,
        endedAt: held.created_at,
        result: "passed",
        report: record.id,
        session: held.author.id,
        ...(model === "" ? {} : { model }),
      })
    );
    since = held.created_at;
  }
  if (!SEEDED_PHASES.some(([ran, word]) => ran === phase && fixture[word] !== undefined)) {
    const run = stepRun(
      fixture.issue,
      GENERATION,
      phase,
      roleToken(project, fixture.issue, phase === "accepting" ? "architect" : STEPS[phase].role),
      1
    );
    await putStep(fixture.issue, run.id, stepStart(run, since));
    written += 1;
  }
  console.log(`seeded ${fixture.issue}'s ${written} runs of steps, ${phase} open`);
}
