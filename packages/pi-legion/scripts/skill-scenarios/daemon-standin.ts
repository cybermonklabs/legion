#!/usr/bin/env bun
/**
 * A stand-in for the Legion daemon that serves exactly the routes a Legion role's `legion` tool and
 * plugin hit in a skill scenario: the claim registration (`claims/register`, `claims/ready`, the
 * daemon's claim routes the plugin boots through), the session's activity report
 * (`claims/activity`, after each turn), the grant the tool mints in-process before each of its own
 * daemon calls (`grants`), the issue's record (`GET /legion/v1/issues/<key>/record`, the tool's
 * `read_record` and the reminder's own check, with `GET /legion/v1/state` beside it), the health
 * route (`GET /healthz`, which the tool's `read_build` reads for the contract this stand-in states
 * on it), the phase completion (`handoff/complete`, the tool's `handoff_complete`), and an owner's
 * routes: `signoff` (`sign_off`), `phase/backward` (`request_backward_move`), `tasks/stop`
 * (`stop_task`), `waves/release` (`release_children`), `gates/register` (`register_gate`) and
 * `plans/register` (`register_plan`). Each of the last eight redeems a grant. No credential route
 * and no grant file: a pane's `gh` and `git` read the rig's token from the gh files under its
 * `GH_CONFIG_DIR`, which `daemon-pane.go` writes, and no agent runs `legion` from bash.
 *
 * Every request and response body is the Go daemon's (`@legion/contracts/legion-api`): each body is
 * parsed with its route's schema, and one that does not parse is answered 400 naming the schema.
 * The grant rule mirrors the real daemon's: a grant lives for 60 seconds and redeems any number of
 * times while it lives; an unknown or expired grant id answers 403
 * `{"error":"Invalid or expired grant"}`. Nothing is single-use. A completion of a phase that ends
 * with a handoff record (daemon-pane.go's handoff word for the role's phase) must name one,
 * `handoffId` and a positive `handoffVersion`, and is refused 400 `HANDOFF_RECORD_REQUIRED`
 * otherwise; a completion of a phase that ends with none and names one is refused 409
 * `HANDOFF_RECORD_MISMATCH`; both as the real daemon refuses them (`L/api/handoff.go`). Any other
 * completion, and every sign-off, move back, task stop, release and gate registration, answers `{}`:
 * the stand-in moves no phase, so an issue's record keeps the phase it started in. The stand-in
 * reads no handoff record from Dispatch: the `legion` tool found the newest record of the phase on
 * Dispatch before posting, and the rig's scorer reads that record back from Dispatch and holds it
 * to the run's session (score.ts testerProof).
 *
 * `register_plan` is answered by the label's own route: the stand-in reads the plan's document from
 * the run's Dispatch (`--dispatch`), its latest version and the blocks of the version given, as the
 * label's Dispatch client reads them (`dispatch.Client.PlanDocument`), and runs
 * `bash <label>/packages/pi-legion/scripts/skill-scenarios/label-texts.sh plan-check <request>
 * <answer>` with that version as C2's plan request, in the stand-in's own environment, so the
 * entry finds the binaries its RIG_TEXTS_BIN names. It answers the entry's answer, its status and
 * its body. Before it reads a block it refuses what the label's route refuses before it reads one
 * (`api/plan.go`, planOwner and documentOfIssue): 403 `ISSUE_OUTSIDE_TREE` for an issue outside the
 * claim's tree (the claim's issue, and each issue of `--issues` in its tree), and 404
 * `ARTIFACT_NOT_ON_ISSUE` for a document Dispatch does not hold, or holds on another issue than the
 * request's. Neither runs the entry. An entry that exits non-zero is answered 500, naming its exit
 * and the last line of its stderr; so is an entry that leaves no answer, an answer whose status no
 * response carries (below 200, or a body on a status that has none), and a stand-in started
 * without `--label` or `--dispatch`: it never answers a plan on its own. A read of the run's
 * Dispatch that fails, and a version whose blocks cannot be read, are answered 502.
 *
 * Every request appends one JSON line to the log file: `{at, path, status, grantId?, sessionId?,
 * mintedGrantId?, handoffId?, handoffVersion?, body?}` (C3's `StandinLogLine`), the run's record of
 * what reached the daemon. A completion's line carries the record it named; the line of an owner's
 * route carries its whole parsed body (score.ts headsArchitect), and an activity report's line the
 * report's activity, `{at, backgroundWork, phase}`, with no claim token, session id or secret.
 *
 * Usage: `bun daemon-standin.ts <port> <log file> <boot token file> <project> <issue> <role>
 * <repository> <default branch> [<phase>] [--tree <root>] [--issues <file>] [--label <checkout>]
 * [--dispatch <url> <token file>]`
 *
 * - The claim is `<role>` of `<issue>` in the tree of `<root>`, `<issue>` itself without `--tree`.
 *   Its issue stands in the phase its role works (daemon-pane.go's `phase`), or in `<phase>` for a
 *   role that works none (the architect, which daemon-pane.go's `phase` refuses), whose phase ends
 *   with no handoff record.
 * - Every issue the state document holds is on `<repository>` and its `<default branch>`: rig.sh's
 *   tester-proof world, or the world an owner's run walks.
 * - `--issues` names the world's issues, `{key, tree, phase, status}` (`StandinIssues`), which the
 *   state and the record route answer beside the claim's. An issue in no phase is one the daemon
 *   records nothing of, so neither names it. A file that gives the claim's issue another tree or
 *   phase than the command line does is refused.
 * - `--label` is the run's label checkout: the `Legion-Daemon-Contract` header states the contract
 *   that label's plugin speaks, and `register_plan` runs its entry. Without it the header states
 *   this checkout's own.
 * - `--dispatch` is the run's Dispatch and the file of its bearer token, which `register_plan`
 *   reads the plan from.
 *
 * A command line it cannot serve exits 2, naming what is wrong. Prints
 * `listening on http://127.0.0.1:<port>` once the socket is open (port 0: the kernel's pick).
 */
import { randomUUID } from "node:crypto";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { appendFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { isLegionRole, roleToken } from "@legion/contracts";
import { daemonContract } from "@legion/contracts/daemon-contract";
import {
  LEGION_PHASES,
  LegionActivityRequest,
  LegionGateRegisterRequest,
  LegionGrantRequest,
  LegionHandoffCompleteRequest,
  LegionHealthResponse,
  LegionPhaseBackwardRequest,
  LegionPlanRegisterRequest,
  LegionSignOffRequest,
  LegionStateResponse,
  LegionTaskStopRequest,
  LegionWaveReleaseRequest,
} from "@legion/contracts/legion-api";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { type ZodSafeParseResult, z } from "zod";
import pkg from "../../package.json";
import { DAEMON_MODULE, runDaemonPane } from "./daemon-pane";
import {
  PlanAnswer,
  PlanRequest,
  type StandinIssue,
  StandinIssues,
  type StandinLogLine,
} from "./scored-runs-contracts";

const GRANT_TTL_MS = 60_000;
const USAGE =
  "usage: bun daemon-standin.ts <port> <log file> <boot token file> <project> <issue> <role> <repository> <default branch> [<phase>] [--tree <root>] [--issues <file>] [--label <checkout>] [--dispatch <url> <token file>]";

function refuse(message: string): never {
  console.error(message);
  process.exit(2);
}

// The command line: eight or nine words, then the options.
const words = Bun.argv.slice(2);
const firstOption = words.findIndex((word) => word.startsWith("--"));
const positional = firstOption < 0 ? words : words.slice(0, firstOption);
if (positional.length < 8 || positional.length > 9) refuse(USAGE);
const [portArg, logFile, bootTokenFile, project, issue, role, repository, defaultBranch] =
  positional;
/** The ninth word: the phase of a role that works none, absent for one that works a phase. */
const phaseArg: string | undefined = positional[8];
const options: {
  tree?: string;
  issues?: string;
  label?: string;
  dispatchUrl?: string;
  dispatchTokenFile?: string;
} = {};
const optionWords = firstOption < 0 ? [] : words.slice(firstOption);
const take = (option: string): string => {
  const value = optionWords.shift();
  if (value === undefined || value.startsWith("--")) refuse(`${option} needs a value; ${USAGE}`);
  return value;
};
while (optionWords.length > 0) {
  const option = optionWords.shift() ?? "";
  switch (option) {
    case "--tree":
      options.tree = take(option);
      break;
    case "--issues":
      options.issues = take(option);
      break;
    case "--label":
      options.label = take(option);
      break;
    case "--dispatch":
      options.dispatchUrl = take(option);
      options.dispatchTokenFile = take(option);
      break;
    default:
      refuse(`${option} is no option of the stand-in; ${USAGE}`);
  }
}
if (!isLegionRole(role)) refuse(`${role} is not a Legion role`);
if (phaseArg !== undefined && !LEGION_PHASES.some((phase) => phase === phaseArg)) {
  refuse(`${phaseArg} is not a phase of the daemon's (${LEGION_PHASES.join(", ")})`);
}

/** Encoded exactly as the daemon encodes it (`legion-<project>-<key>-<role>`, lower-cased),
 * since the worker claims this token on the real Envoy and Envoy rejects uppercase. */
const ROLE_TOKEN = roleToken(project, issue, role);
/** The worker's registration secret: random, as the daemon's is, since the pane holds it and
 * scrubs it from every line it prints (`@legion/contracts/held-secrets`). */
const SESSION_SECRET = fixtureSecret("reg");
/** The root of the claim's tree. */
const TREE = options.tree ?? issue;

const startedAt = new Date().toISOString();
/** The phase the issue is in and the word of the handoff record it ends with, empty when none: the
 * phase the role works, or the one the ninth argument names, with no handoff record. */
const [PHASE = "", HANDOFF_WORD = ""] =
  phaseArg === undefined
    ? runDaemonPane(DAEMON_MODULE, ["phase", role]).split("\n")
    : [phaseArg, ""];

/** The label's plugin as far as the stand-in reads it: the contract its manifest states, which a
 *  manifest in a checkout leaves out from the daemon API contract cutover on. */
const LabelPlugin = z.object({
  legion: z.object({ daemonApiVersion: z.number().int().positive().optional() }).optional(),
});

/** The contract the `Legion-Daemon-Contract` header states: the label's plugin's, or this
 * checkout's own without `--label`. Each is `daemonContract` of that plugin's manifest from its
 * own directory: the manifest's `legion.daemonApiVersion` where it states one, else the frozen
 * list's greatest number plus the count of entry fixtures of the checkout that holds it. That is
 * the number the label's install writes into the plugin it packs. */
const DAEMON_CONTRACT = String(
  options.label === undefined ? daemonContract(pkg, import.meta.dir) : labelContract(options.label)
);

function labelContract(checkout: string): number {
  const plugin = path.join(checkout, "packages", "pi-legion");
  const file = path.join(plugin, "package.json");
  try {
    return daemonContract(LabelPlugin.parse(JSON.parse(readFileSync(file, "utf8"))), plugin);
  } catch (error) {
    refuse(`--label ${checkout}: its plugin states no contract (${file}): ${error}`);
  }
}

/** The label checkout whose entry answers `register_plan`. */
const LABEL = options.label;

/** The run's Dispatch, which `register_plan` reads the plan from, and its bearer token. */
const DISPATCH =
  options.dispatchUrl === undefined
    ? undefined
    : dispatchOf(options.dispatchUrl, options.dispatchTokenFile ?? "");

function dispatchOf(
  url: string,
  tokenFile: string
): { readonly url: string; readonly token: string } {
  if (!URL.canParse(url)) refuse(`--dispatch ${url} is no URL`);
  let token: string;
  try {
    token = readFileSync(tokenFile, "utf8").trim();
  } catch (error) {
    refuse(`--dispatch: cannot read the token file ${tokenFile}: ${error}`);
  }
  if (token === "") refuse(`--dispatch: the token file ${tokenFile} is empty`);
  return { url: url.replace(/\/+$/, ""), token };
}

/** The world's issues of `--issues`, none without it. */
const WORLD = options.issues === undefined ? [] : worldIssues(options.issues);

function worldIssues(file: string): StandinIssue[] {
  try {
    return StandinIssues.parse(JSON.parse(readFileSync(file, "utf8")));
  } catch (error) {
    refuse(`--issues ${file}: ${error}`);
  }
}

const claimInWorld = WORLD.find((each) => each.key === issue);
if (claimInWorld !== undefined && (claimInWorld.tree !== TREE || claimInWorld.phase !== PHASE)) {
  refuse(
    `--issues ${options.issues} gives the claim's issue ${issue} the tree ${claimInWorld.tree} and the phase ${claimInWorld.phase}, and the command line gives it the tree ${TREE} and the phase ${PHASE}`
  );
}

/** The issues the daemon records: the claim's, then each issue of the world in a phase. */
const RECORDED = [
  { key: issue, tree: TREE, phase: PHASE, status: claimInWorld?.status ?? "in_progress" },
  ...WORLD.flatMap((each) =>
    each.key === issue || each.phase === null ? [] : [{ ...each, phase: each.phase }]
  ),
];

/** The issues of the claim's tree: the claim's own, and each issue of the world in that tree. */
const TREE_ISSUES: ReadonlySet<string> = new Set([
  issue,
  ...WORLD.filter((each) => each.tree === TREE).map((each) => each.key),
]);

/** `GET /legion/v1/state`: the issues the daemon records, each admitted in its phase, in its tree,
 * on the repository and default branch the command line names; the claim's in PHASE (the phase
 * its role works, `workflow.RoleFor` through daemon-pane.go, or the ninth argument's).
 * `capabilities` is the deployment's capability report, `strandedClaims` the claims nothing will
 * wake, `heldOutboxRows` the lifecycle rows held behind their issues' running ones, `consumers`
 * the lag of the daemon's durable consumers and `repositories` the repositories the live records
 * name, each of which the reader requires;
 * the rig has no deployment to report on, its one claim runs, it has no outbox and no NATS, so
 * the first four are the empty lists the daemon's MarshalJSON emits when there is none.
 * `wakesWaiting`, the wakes the pacing holds until their turn, is optional to the reader; the rig
 * carries it empty, as a daemon at this contract serves a caller in no pod holding no wake.
 * `settings` is the daemon's resolved legion.yaml with no credential in it, also required; the rig
 * has no file, so it carries the two keys the reader types at the daemon's defaults. */
const stateParse = LegionStateResponse.safeParse({
  daemon: { project, schemaVersion: 1, boots: 1, firstBootAt: startedAt, startedAt },
  admission: {
    cap: RECORDED.length,
    active: RECORDED.map((each) => each.key),
    waiting: [],
    trees: [...new Set(RECORDED.map((each) => each.tree))],
    blocked: [],
  },
  issues: Object.fromEntries(
    RECORDED.map((each) => [
      each.key,
      {
        key: each.key,
        generation: 1,
        phase: each.phase,
        status: each.status,
        tree: each.tree,
        repo: repository,
        defaultBranch,
        workers: {},
      },
    ])
  ),
  pendingStatusWrites: [],
  heldOutboxRows: [],
  strandedClaims: [],
  wakesWaiting: [],
  capabilities: [],
  repositories: {
    live: [{ repo: repository, apps: { implement: true, review: true } }],
    withoutWebhookApp: [],
    caches: [],
  },
  settings: { review_round_cap: 3, worker_boot_timeout_seconds: 120 },
  consumers: [],
});
if (!stateParse.success) {
  refuse(`the state document this command line makes is no state: ${stateParse.error.message}`);
}
const STATE = stateParse.data;

/** `GET /healthz`: that this stand-in is serving, and the project its state names, which is the
 * one word the controller's boot check reads. The contract goes in the header beside it
 * (below), where the tool's `read_build` reads it. */
const HEALTH = LegionHealthResponse.parse({ status: "ok", project });

/** The response header the daemon states its own contract in
 * (`packages/daemon/internal/api/server.go`). */
const DAEMON_CONTRACT_HEADER = "Legion-Daemon-Contract";

/** The entry of the label's own code, under the label's checkout (C2). */
const ENTRY = path.join("packages", "pi-legion", "scripts", "skill-scenarios", "label-texts.sh");

const expectedBootToken = (await Bun.file(bootTokenFile).text()).trim();
if (expectedBootToken.length === 0) refuse(`boot token file ${bootTokenFile} is empty`);

const grants = new Map<string, { readonly expiresAt: number }>();

function json(status: number, body: unknown): Response {
  return Response.json(body, { status });
}

function forbidden(message: string): Response {
  return json(403, { error: message });
}

/** A workflow refusal in the daemon's `Failure` shape, `{code, error}` (`writeFailure`). */
function failure(status: number, code: string, message: string): Response {
  return json(status, { code, error: message });
}

function stringField(body: unknown, key: string): string | undefined {
  if (typeof body !== "object" || body === null) return undefined;
  const value = (body as Record<string, unknown>)[key];
  return typeof value === "string" ? value : undefined;
}

function numberField(body: unknown, key: string): number | undefined {
  if (typeof body !== "object" || body === null) return undefined;
  const value = (body as Record<string, unknown>)[key];
  return typeof value === "number" ? value : undefined;
}

/** A body parsed with its route's schema, or the 400 that names the schema it is not. */
function parseBody<T>(
  schema: string,
  result: ZodSafeParseResult<T>
): { readonly data: T } | { readonly refusal: Response } {
  return result.success
    ? { data: result.data }
    : { refusal: json(400, { error: `the body is no ${schema}: ${result.error.message}` }) };
}

/** Mirrors the real daemon's rule: present and not yet expired, nothing else. Each route parses
 * its body with its own contract schema first, exactly as the daemon's route table does, and
 * hands the parsed id here. */
function resolveGrant(grantId: string): Response | undefined {
  const grant = grants.get(grantId);
  if (!grant || grant.expiresAt <= Date.now()) return forbidden("Invalid or expired grant");
  return undefined;
}

/** What Dispatch's document read answers (`GET /api/v1/artifacts/{id}`), as far as the stand-in
 * reads it: the issue the document is on, and its latest version. */
const DispatchDocument = z.object({
  issue_key: z.string(),
  approval: z.object({ latest_version: z.number().int().positive() }),
});

/** What Dispatch's version read answers (`GET /api/v1/artifacts/{id}/versions/{n}?blocks=true`):
 * the version's markdown, its number, and each typed block's byte range in that markdown and its
 * attributes. A Dispatch that predates the blocks answers no `blocks`. */
const DispatchVersion = z.object({
  markdown: z.string(),
  number: z.number().int(),
  blocks: z
    .array(
      z.object({
        id: z.string().min(1),
        type: z.string().min(1),
        from: z.number().int(),
        to: z.number().int(),
        attributes: z.array(z.object({ name: z.string().min(1), value: z.string() })).nullish(),
      })
    )
    .nullish(),
});

/** One read of the run's Dispatch with its bearer, parsed with `schema`, or the 502 naming what
 * Dispatch answered, and whether Dispatch answered that it has no such thing (404). */
async function dispatchRead<T>(
  dispatch: { readonly url: string; readonly token: string },
  route: string,
  schema: z.ZodType<T>
): Promise<{ readonly data: T } | { readonly refusal: Response; readonly missing: boolean }> {
  let answered: Response;
  try {
    answered = await fetch(`${dispatch.url}${route}`, {
      headers: { Accept: "application/json", Authorization: `Bearer ${dispatch.token}` },
    });
  } catch (error) {
    return {
      refusal: failure(502, "DISPATCH_FAILED", `the read of GET ${route} failed: ${error}`),
      missing: false,
    };
  }
  const text = await answered.text();
  let parsed: ZodSafeParseResult<T> | undefined;
  try {
    parsed = answered.ok ? schema.safeParse(JSON.parse(text)) : undefined;
  } catch {
    parsed = undefined;
  }
  if (parsed === undefined || !parsed.success) {
    return {
      refusal: failure(
        502,
        "DISPATCH_FAILED",
        `the run's Dispatch answered GET ${route} with ${answered.status} and a body the stand-in cannot read: ${text}`
      ),
      missing: answered.status === 404,
    };
  }
  return { data: parsed.data };
}

/** The blocks of one version as the label's Dispatch client cuts them (`PlanDocument`): each
 * block's text is its body, the bytes of its range without its opening line and its closing
 * fence. */
function cutBlocks(
  version: z.infer<typeof DispatchVersion>,
  asked: number
): { readonly blocks: PlanRequest["blocks"] } | { readonly error: string } {
  if (version.number !== asked) return { error: `Dispatch answered version ${version.number}` };
  if (version.blocks == null) return { error: "Dispatch answered the version without its blocks" };
  const markdown = Buffer.from(version.markdown, "utf8");
  const blocks: PlanRequest["blocks"] = [];
  for (const block of version.blocks) {
    if (block.from < 0 || block.to < block.from || block.to > markdown.length) {
      return {
        error: `block ${block.id} spans ${block.from} to ${block.to} of ${markdown.length} bytes`,
      };
    }
    const rendering = markdown.subarray(block.from, block.to).toString("utf8");
    const opening = rendering.indexOf("\n");
    const rest = rendering.slice(opening + 1);
    const closing = rest.lastIndexOf("\n");
    if (opening < 0 || closing < 0) return { error: `block ${block.id} is no fenced block` };
    blocks.push({
      id: block.id,
      type: block.type,
      attributes: Object.fromEntries(
        (block.attributes ?? []).map((attribute) => [attribute.name, attribute.value])
      ),
      text: rest.slice(0, closing),
    });
  }
  return { blocks };
}

/** `register_plan`'s answer: the version from the run's Dispatch, given to the label's own route
 * through the entry, once the document is the request's issue's (documentOfIssue's rule). */
async function planCheck(request: {
  readonly issue: string;
  readonly artifactId: string;
  readonly version: number;
}): Promise<Response> {
  if (LABEL === undefined || DISPATCH === undefined) {
    const missing = [
      LABEL === undefined ? "--label" : "",
      DISPATCH === undefined ? "--dispatch" : "",
    ]
      .filter((option) => option !== "")
      .join(" and ");
    return failure(
      500,
      "PLAN_CHECK_UNAVAILABLE",
      `this stand-in was started without ${missing}, so it has no label's register_plan to ask and no plan to give it; it answers no plan on its own`
    );
  }
  const artifactId = request.artifactId.toLowerCase();
  const documentRoute = `/api/v1/artifacts/${encodeURIComponent(artifactId)}`;
  const document = await dispatchRead(DISPATCH, documentRoute, DispatchDocument);
  const notOnIssue = failure(
    404,
    "ARTIFACT_NOT_ON_ISSUE",
    `${artifactId} is not a document of ${request.issue}`
  );
  if ("refusal" in document) return document.missing ? notOnIssue : document.refusal;
  if (document.data.issue_key !== request.issue) return notOnIssue;
  const latestVersion = document.data.approval.latest_version;
  // The label's route refuses a version above the latest before it reads any block, so none is
  // read for one.
  let blocks: PlanRequest["blocks"] = [];
  if (request.version <= latestVersion) {
    const route = `${documentRoute}/versions/${request.version}?blocks=true`;
    const version = await dispatchRead(DISPATCH, route, DispatchVersion);
    if ("refusal" in version) return version.refusal;
    const cut = cutBlocks(version.data, request.version);
    if ("error" in cut) {
      return failure(
        502,
        "PLAN_UNREADABLE",
        `version ${request.version} of ${artifactId} on the run's Dispatch: ${cut.error}`
      );
    }
    blocks = cut.blocks;
  }
  return runEntry(
    LABEL,
    PlanRequest.parse({ artifactId, version: request.version, latestVersion, blocks })
  );
}

/** The entry's answer to one plan request: its status and its body, or the 500 naming its exit and
 * the last line of its stderr. Its output goes to this process's stderr, the rig's log of it. */
async function runEntry(label: string, request: PlanRequest): Promise<Response> {
  const scratch = mkdtempSync(path.join(os.tmpdir(), "daemon-standin-plan-"));
  try {
    const requestFile = path.join(scratch, "request.json");
    const answerFile = path.join(scratch, "answer.json");
    writeFileSync(requestFile, JSON.stringify(request));
    const entry = Bun.spawn(
      ["bash", path.join(label, ENTRY), "plan-check", requestFile, answerFile],
      { cwd: label, stdin: "ignore", stdout: 2, stderr: "pipe" }
    );
    const [exit, stderr] = await Promise.all([entry.exited, new Response(entry.stderr).text()]);
    process.stderr.write(stderr);
    if (exit !== 0) {
      const last = stderr.trimEnd().split("\n").at(-1)?.trim() || "nothing on stderr";
      const reason = `label-texts.sh plan-check exited ${exit}: ${last}`;
      console.error(`daemon-standin: register_plan answered 500: ${reason}`);
      return failure(500, "PLAN_CHECK_FAILED", reason);
    }
    let answer: PlanAnswer;
    try {
      answer = PlanAnswer.parse(JSON.parse(readFileSync(answerFile, "utf8")));
    } catch (error) {
      const reason = `label-texts.sh plan-check exited 0 and left no plan answer: ${error}`;
      console.error(`daemon-standin: register_plan answered 500: ${reason}`);
      return failure(500, "PLAN_CHECK_FAILED", reason);
    }
    // A status below 200 is no answer the Fetch standard's `Response` takes; Bun sends it anyway
    // (a 101 reaches the client as an upgrade), so it is refused here, as is a body on a status
    // that has none.
    if (answer.status < 200 || (answer.body !== "" && BODILESS_STATUSES.has(answer.status))) {
      const reason = `the label's register_plan answered status ${answer.status} with a body of ${answer.body.length} characters, which no response carries`;
      console.error(`daemon-standin: register_plan answered 500: ${reason}`);
      return failure(500, "PLAN_CHECK_FAILED", reason);
    }
    return new Response(answer.body === "" ? null : answer.body, {
      status: answer.status,
      headers: { "Content-Type": "application/json" },
    });
  } finally {
    rmSync(scratch, { recursive: true, force: true });
  }
}

/** The statuses whose response has no body (the Fetch standard's null body statuses that a
 *  response can be made with). */
const BODILESS_STATUSES: ReadonlySet<number> = new Set([204, 205, 304]);

/** What one request is answered, and what its log line keeps beside the request's own fields: the
 * grant a grant request minted, and `body`. */
interface Answer {
  readonly response: Response;
  readonly mintedGrantId?: string;
  readonly logged?: Record<string, unknown>;
}

/** The owner's routes the stand-in answers `{}`, each body parsed with its route's schema;
 * undefined for any other route. */
function ownerBody(
  route: string,
  body: unknown
):
  | { readonly data: { readonly grantId: string } & Record<string, unknown> }
  | { readonly refusal: Response }
  | undefined {
  switch (route) {
    case "/legion/v1/signoff":
      return parseBody("LegionSignOffRequest", LegionSignOffRequest.safeParse(body));
    case "/legion/v1/phase/backward":
      return parseBody("LegionPhaseBackwardRequest", LegionPhaseBackwardRequest.safeParse(body));
    case "/legion/v1/tasks/stop":
      return parseBody("LegionTaskStopRequest", LegionTaskStopRequest.safeParse(body));
    case "/legion/v1/waves/release":
      return parseBody("LegionWaveReleaseRequest", LegionWaveReleaseRequest.safeParse(body));
    case "/legion/v1/gates/register":
      return parseBody("LegionGateRegisterRequest", LegionGateRegisterRequest.safeParse(body));
    default:
      return undefined;
  }
}

async function handle(route: string, body: unknown): Promise<Answer> {
  switch (route) {
    case "/legion/v1/claims/register": {
      if (stringField(body, "bootToken") !== expectedBootToken) {
        return { response: forbidden("Invalid boot token") };
      }
      return {
        response: json(200, {
          claimToken: ROLE_TOKEN,
          tree: TREE,
          issue,
          role,
          generation: 1,
          secret: SESSION_SECRET,
          promptAgents: [],
        }),
      };
    }
    case "/legion/v1/claims/ready": {
      if (stringField(body, "secret") !== SESSION_SECRET) {
        return { response: forbidden("Invalid session secret") };
      }
      return { response: new Response(null, { status: 204 }) };
    }
    case "/legion/v1/claims/activity": {
      const parsed = parseBody("LegionActivityRequest", LegionActivityRequest.safeParse(body));
      if ("refusal" in parsed) return { response: parsed.refusal };
      const logged = parsed.data.activity;
      if (parsed.data.secret !== SESSION_SECRET) {
        return { response: forbidden("Invalid session secret"), logged };
      }
      return { response: new Response(null, { status: 204 }), logged };
    }
    case "/legion/v1/grants": {
      const parsed = parseBody("LegionGrantRequest", LegionGrantRequest.safeParse(body));
      if ("refusal" in parsed) return { response: parsed.refusal };
      if (parsed.data.secret !== SESSION_SECRET) {
        return { response: forbidden("Invalid session secret") };
      }
      const grantId = randomUUID();
      const expiresAt = Date.now() + GRANT_TTL_MS;
      grants.set(grantId, { expiresAt });
      return {
        response: json(200, { grantId, expiresAt: new Date(expiresAt).toISOString() }),
        mintedGrantId: grantId,
      };
    }
    case "/legion/v1/handoff/complete": {
      const parsed = parseBody(
        "LegionHandoffCompleteRequest",
        LegionHandoffCompleteRequest.safeParse(body)
      );
      if ("refusal" in parsed) return { response: parsed.refusal };
      const refused = resolveGrant(parsed.data.grantId);
      if (refused) return { response: refused };
      const { handoffId, handoffVersion } = parsed.data;
      if (HANDOFF_WORD !== "" && (handoffId === undefined || handoffVersion === undefined)) {
        const write = `\`dispatch handoff-write --issue ${issue} --phase ${HANDOFF_WORD} --body-file <file>\``;
        return {
          response: failure(
            400,
            "HANDOFF_RECORD_REQUIRED",
            `phase ${PHASE} of ${issue} ends with its ${HANDOFF_WORD} handoff recorded on the Dispatch issue, and this completion names no record (handoffId and a positive handoffVersion): write it with ${write}, then complete again; this completion changed nothing`
          ),
        };
      }
      if (HANDOFF_WORD === "" && (handoffId !== undefined || handoffVersion !== undefined)) {
        return {
          response: failure(
            409,
            "HANDOFF_RECORD_MISMATCH",
            `phase ${PHASE} of ${issue} ends with no handoff record, and this completion names one: complete without handoffId and handoffVersion; this completion changed nothing`
          ),
        };
      }
      return { response: json(200, {}) };
    }
    case "/legion/v1/plans/register": {
      const parsed = parseBody(
        "LegionPlanRegisterRequest",
        LegionPlanRegisterRequest.safeParse(body)
      );
      if ("refusal" in parsed) return { response: parsed.refusal };
      // The route's order: the grant, the issue's tree (planOwner), then the document's issue.
      const refused =
        resolveGrant(parsed.data.grantId) ??
        (TREE_ISSUES.has(parsed.data.issue)
          ? undefined
          : failure(
              403,
              "ISSUE_OUTSIDE_TREE",
              `${parsed.data.issue} is outside the architect tree ${TREE}`
            ));
      return { response: refused ?? (await planCheck(parsed.data)), logged: parsed.data };
    }
    default: {
      const owner = ownerBody(route, body);
      if (owner === undefined) return { response: json(404, { error: `no route for ${route}` }) };
      if ("refusal" in owner) return { response: owner.refusal };
      return { response: resolveGrant(owner.data.grantId) ?? json(200, {}), logged: owner.data };
    }
  }
}

/** `GET /legion/v1/issues/<key>/record`, and the key it names. */
const RECORD_ROUTE = /^\/legion\/v1\/issues\/([^/]+)\/record$/;

/** The answer to one request: the POST routes `handle` serves, then the three GET routes, then
 * the 404 every other request gets. */
async function answer(method: string, route: string, body: unknown): Promise<Answer> {
  if (method === "POST") return handle(route, body);
  if (method === "GET") {
    if (route === "/healthz") {
      return {
        response: Response.json(HEALTH, {
          headers: { [DAEMON_CONTRACT_HEADER]: DAEMON_CONTRACT },
        }),
      };
    }
    if (route === "/legion/v1/state") return { response: json(200, STATE) };
    const record = RECORD_ROUTE.exec(route);
    if (record !== null) {
      const held = STATE.issues?.[decodeURIComponent(record[1] ?? "")];
      return {
        response:
          held === undefined
            ? json(404, {
                code: "RECORD_NOT_FOUND",
                error: `The daemon has no record for ${record[1]}`,
              })
            : json(200, held),
      };
    }
  }
  return { response: json(404, { error: `no route for ${method} ${route}` }) };
}

const server = Bun.serve({
  hostname: "127.0.0.1",
  port: Number(portArg),
  // register_plan waits on the label's entry, which runs a test binary, for longer than Bun's
  // default of ten idle seconds.
  idleTimeout: 0,
  async fetch(request) {
    const route = new URL(request.url).pathname;
    let body: unknown;
    if (request.method === "POST") {
      try {
        body = await request.json();
      } catch {
        body = undefined;
      }
    }
    const { response, mintedGrantId, logged } = await answer(request.method, route, body);
    const grantId = stringField(body, "grantId");
    const sessionId = stringField(body, "sessionId");
    const handoffId = stringField(body, "handoffId");
    const handoffVersion = numberField(body, "handoffVersion");
    const line: StandinLogLine = {
      at: new Date().toISOString(),
      path: route,
      status: response.status,
      ...(grantId === undefined ? {} : { grantId }),
      ...(sessionId === undefined ? {} : { sessionId }),
      ...(mintedGrantId === undefined ? {} : { mintedGrantId }),
      ...(handoffId === undefined ? {} : { handoffId }),
      ...(handoffVersion === undefined ? {} : { handoffVersion }),
      ...(logged === undefined ? {} : { body: logged }),
    };
    await appendFile(logFile, `${JSON.stringify(line)}\n`);
    return response;
  },
});

console.log(`listening on http://127.0.0.1:${server.port}`);
