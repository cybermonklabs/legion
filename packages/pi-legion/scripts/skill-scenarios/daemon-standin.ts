#!/usr/bin/env bun
/**
 * A stand-in for the Legion daemon that serves exactly the routes a Legion role's `legion` tool
 * and plugin hit in a skill scenario: the claim's registration (`claims/register`, `claims/ready`,
 * the daemon's claim routes the plugin boots through), the pane's turn-boundary reports
 * (`claims/activity` and `claims/turn-outcome`, after each turn), what the claim owes
 * (`claims/owed`, which the phase reminder and the stall watch ask), the grant the tool mints
 * in-process before each of its own daemon calls (`grants`, in its session form for a phase worker
 * or an architect and its controller form for the controller), the issue's record
 * (`GET /legion/v1/issues/<key>/record`, the tool's `read_record`) and the state
 * (`GET /legion/v1/state`, the controller's `read_state`), the health route (`GET /healthz`, which
 * the tool's `read_build` reads for the contract this stand-in states on it), and the writes each
 * role's operations make, every one of which redeems a grant:
 *
 *   phase worker   `handoff_complete` (`handoff/complete`, a step's completion or a production
 *                  check's look report) and `request_backward_move` (`phase/backward`)
 *   architect      those two, and `register_gate` (`gates/register`), `register_plan`
 *                  (`plans/register`), `sign_off` (`signoff`), `release_children`
 *                  (`waves/release`), `rerun_child` (`children/rerun`) and `stop_task`
 *                  (`tasks/stop`)
 *   controller     `set_status` (`issues/status`), and `read_state`
 *
 * No credential route and no grant file: a pane's `gh` and `git` read the rig's token from the gh
 * files under its `GH_CONFIG_DIR`, which `daemon-pane.go` writes, and no agent runs `legion` from
 * bash. A route the `legion` tool has no operation of for these roles (read_release_records,
 * ready_for_update, retry_or_escalate, close_root, the controller's other writes) is answered 404
 * unless an answers file holds an answer for it.
 *
 * Every request body is parsed with its contract schema (`@legion/contracts/legion-api`), as the
 * daemon's route table parses it, and one that does not parse is refused 400 naming the schema.
 * The grant rule mirrors the real daemon's: a grant lives for 60 seconds and redeems any number of
 * times while it lives; an unknown or expired grant id answers 403
 * `{"error":"Invalid or expired grant"}`. Nothing is single-use. A request that parses and holds a
 * live grant is answered, in this order, from:
 *
 *   1. the answers files. `--case-answers` (the case's own `daemon-answers.json`) and `--answers`
 *      (the shared one), each `{version: 1, captured: {command, at, commit}, answers: [{name,
 *      method, path, match, status, body}]}` as t4-contracts.md defines it: the first entry, the
 *      case's before the shared file's, whose `method` and `path` equal the request's and whose
 *      every `match` value equals the request's at its dot path (`args.wake_at`, `issues.0`; a
 *      `null` holds where the request has no value there) answers it with its `status` and `body`
 *      (a 204 answers no body). A file or an entry with a member this schema does not name is
 *      refused when the stand-in starts. A production check's look report is never answered from
 *      a file: each entry was captured against one state of a check, and the stand-in holds the
 *      check, whose state each report moves.
 *   2. the built-in answer: the daemon's own refusals the case's world makes (below), then `{}`.
 *
 * The built-in refusals of `handoff/complete`, each the daemon's own code and status
 * (`packages/daemon/internal/api/handoff.go`): a production check's look report is held to the
 * check the world has open (CHECK_NOT_OPEN, CHECK_ROLE_MISMATCH, CHECK_VERDICT_REQUIRED,
 * CHECK_WAKE_REQUIRED, CHECK_WAKE_UNEXPECTED, CHECK_PHASE_FIELDS_UNEXPECTED,
 * PLAN_RATING_UNEXPECTED, RETURN_TO_NEEDS_FAIL) and a repeat of a report is HANDOFF_ALREADY_RECORDED;
 * a completion of a done issue is CHECK_LOOK_NOT_SERVED; an architect's is ACCEPTANCE_IS_SIGN_OFF; a
 * role's of a phase it does not work is HANDOFF_NOT_CURRENT_PHASE; a phase that ends with a handoff
 * record (daemon-pane.go's handoff word for the phase) must name one, `handoffId` and a positive
 * `handoffVersion`, and is refused 400 HANDOFF_RECORD_REQUIRED otherwise, and a phase that ends
 * with none and names one is refused 409 HANDOFF_RECORD_MISMATCH; the tester's verdict is pass or
 * fail and no other role's names one; a tester's fail names a step before the one it stands at
 * (RETURN_REFUSED, from the world's `daemon.step`); the implementer's completion of implementing
 * needs an open pull request on `legion/<issue>` (HANDOFF_NO_PULL_REQUEST: one the world holds, or
 * one gh-standin recorded opening on the branch, `open` in a line's `pull` in `--gh-calls`, and no
 * recorded close or merge of it since). The daemon's other refusals depend on state this
 * stand-in does not hold (the claim's serving run and entry, Dispatch's records, GitHub): a
 * recorded answer (`--answers`) stands for each. `request_backward_move` of another issue than a
 * phase worker's own is refused 400 ISSUE_NOT_GRANTS, `register_gate` of a child 403
 * GATE_ROOT_ONLY, and `set_status` without a controller grant 403 CONTROLLER_REQUIRED. The stand-in
 * reads no handoff record from Dispatch: the `legion` tool found the newest record of the phase on
 * Dispatch before posting, and the rig's scorer reads that record back from Dispatch and holds it
 * to the run's session (score.ts testerProof).
 *
 * `claims/owed` is answered from the record this stand-in holds, as the daemon's `owedBy`
 * (`packages/daemon/internal/api/owed.go`) reads its own: the architect and the controller owe
 * nothing and name no architect; a phase worker owes the production check while the record holds
 * the check's look open for its role, else the phase the record holds its issue in while its role
 * works that phase and no completion of it has been accepted since the phase was set, by the
 * built-in answer or a recorded one; a merger owes nothing once the world holds its pull request
 * merged. Each phase worker's answer names the tree root's architect. An accepted completion moves
 * no phase of the record, which the daemon's workflow would move on.
 *
 * A start with a case's world (`--world`) replays the case. Any other start is the stand-in of a
 * scored run, an owner's (rig.sh scored_session), or of rig.sh's tester-proof run, and differs in
 * three things:
 *
 * - `register_plan`: a case's is answered `{plan: {version}}`. A scored run's is answered by the
 *   label's own route: the stand-in reads the plan's document from the run's Dispatch
 *   (`--dispatch`), its latest version and the blocks of the version given, as the label's Dispatch
 *   client reads them (`dispatch.Client.PlanDocument`), and runs
 *   `bash <label>/packages/pi-legion/scripts/skill-scenarios/label-texts.sh plan-check <request>
 *   <answer>` with that version as C2's plan request, in the stand-in's own environment, so the
 *   entry finds the binaries its RIG_TEXTS_BIN names. It answers the entry's answer, its status and
 *   its body. Before it reads a block it refuses what the label's route refuses before it reads one
 *   (`api/plan.go`, planOwner and documentOfIssue): 403 `ISSUE_OUTSIDE_TREE` for an issue outside
 *   the claim's tree (the claim's issue, and each issue of `--issues` in its tree), and 404
 *   `ARTIFACT_NOT_ON_ISSUE` for a document Dispatch does not hold, holds on no issue, or holds on
 *   another issue than the request's. Neither runs the entry. An entry that exits non-zero is
 *   answered 500, naming its exit and the last line of its stderr; so is an entry that leaves no
 *   answer, an answer whose status no response carries (below 200, or a body on a status that has
 *   none), and a stand-in started without `--label` or `--dispatch`: it never answers a plan on
 *   its own. A read of the run's Dispatch that fails, and a version whose blocks cannot be read,
 *   are answered 502.
 * - `release_children`: a case's is answered with the issues it released, each once; a scored
 *   run's with `{}`.
 * - The log line. Every request appends one JSON line to the log file. A case's is `{at, method,
 *   path, status, body, answer, grantId?, sessionId?, handoffId?, handoffVersion?}`: `body` is the
 *   request JSON (null for a GET or a body that is no JSON) and `answer` the response JSON (null
 *   where the answer has no body, a 204), each with the value of a `secret` or `bootToken` member
 *   replaced by "<redacted>". A scored run's is C3's `StandinLogLine`, `{at, path, status,
 *   grantId?, sessionId?, mintedGrantId?, handoffId?, handoffVersion?, body?}`: the line of an
 *   owner's route carries its whole parsed body (score.ts headsArchitect), and an activity
 *   report's line the report's activity, `{at, backgroundWork, phase}`, with no claim token,
 *   session id or secret. Either line carries the record a completion named.
 *
 * Usage: `bun daemon-standin.ts <port> <log file> <boot token file> <project> <issue> <role>
 * <repository> <default branch> [<phase>] [--tree <root>] [--world <file>] [--clock-real <file>]
 * [--answers <file>] [--case-answers <file>] [--gh-calls <file>] [--issues <file>]
 * [--label <checkout>] [--dispatch <url> <token file>]`
 *
 * - `<role>` is a Legion role or `controller`. The claim is `<role>` of `<issue>` in the tree of
 *   `<root>`, `<issue>` itself without `--tree` (the issue is a root).
 * - The claim's issue stands in `<phase>` for a role that works none (the architect, which
 *   daemon-pane.go's `phase` refuses), whose phase ends with no handoff record; else in the phase
 *   the case's world stands it in; else in the phase its role works (daemon-pane.go's `phase`).
 * - Every issue the state document holds is on `<repository>` and its `<default branch>`: rig.sh's
 *   tester-proof world, a case's, or the world an owner's run walks.
 * - `--world` names a JSON file holding a case's `world` whole (heldout-case.ts `World`, parsed
 *   strictly), of which the stand-in reads `daemon.phase`, the `dispatch.issue.status`, the
 *   issue's pull request among `github.pullRequests` (the record's `pullRequest`), `daemon.step`
 *   (the place a tester's `return_to` is held above) and `daemon.productionCheck` (the record's
 *   open `check` on a done issue: its look, whether that look is the last, when it opened, its
 *   bound and its merge commit; the tester works it). `daemon.task` is the text of the worker's
 *   task, which the daemon sends and its state does not carry, so the rig writes it into the
 *   prompt. `--gh-calls` is the run's `calls.jsonl`, which gh-standin.ts writes.
 * - `--clock-real` names the file of the launch's wall-clock instant, the case's clock-real.txt,
 *   which rig.sh writes after the stand-in starts and before the role launches. The case's now is
 *   then the world's clock moved on by the time since that instant, as outcome.ts and lane D's
 *   replay put each record of the run on the case's timeline, so the setup before the launch moves
 *   no wake. The stand-in reads the file when a look report needs the case's now. While the file
 *   cannot be read or holds no RFC 3339 time, it answers that report 500 CASE_CLOCK_UNREADABLE and
 *   records nothing. Without `--clock-real`, the case's now moves on from the stand-in's start.
 *   `--clock-real` is refused without `--world`.
 * - `--issues` names the world's issues, `{key, tree, phase, status}` (`StandinIssues`), which the
 *   state and the record route answer beside the claim's. An issue in no phase is one the daemon
 *   records nothing of, so neither names it. A file that gives the claim's issue another tree or
 *   phase than the command line does is refused.
 * - `--label` is the run's label checkout: the `Legion-Daemon-Contract` header states the contract
 *   that label's plugin speaks, and `register_plan` runs its entry. Without it the header states
 *   this checkout's own.
 * - `--dispatch` is the run's Dispatch and the file of its bearer token, which `register_plan`
 *   reads the plan from.
 * - `--issues`, `--label` and `--dispatch` are a scored run's, and are refused beside `--world`.
 *
 * The document is the world as the run starts: a backward move sets its phase, `set_status` its
 * status, and each look report its check, as the daemon's workflow does (`checkReport` below);
 * nothing else moves it. A command line it cannot serve exits 2, naming what is wrong. Prints
 * `listening on http://127.0.0.1:<port>` once the socket is open (port 0: the kernel's pick).
 */
import { randomUUID } from "node:crypto";
import {
  appendFileSync,
  existsSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import os from "node:os";
import path from "node:path";
import { controllerToken, isLegionRole, type LegionRole, roleToken } from "@legion/contracts";
import { daemonContract } from "@legion/contracts/daemon-contract";
import {
  LEGION_PHASES,
  LegionActivityRequest,
  LegionChildRequest,
  LegionControllerGrantRequest,
  LegionGateRegisterRequest,
  LegionGrantRequest,
  LegionHandoffCompleteRequest,
  LegionHealthResponse,
  LegionIssueRecordResponse,
  LegionIssueStatusRequest,
  type LegionOwed,
  LegionOwedRequest,
  LegionOwedResponse,
  LegionPhaseBackwardRequest,
  LegionPlanRegisterRequest,
  LegionReadyRequest,
  LegionSignOffRequest,
  LegionStateResponse,
  LegionTaskStopRequest,
  LegionTurnOutcomeRequest,
  LegionWaveReleaseRequest,
} from "@legion/contracts/legion-api";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { type ZodSafeParseResult, z } from "zod";
import pkg from "../../package.json";
import { DAEMON_MODULE, runDaemonPane } from "./daemon-pane";
import { World } from "./heldout-case";
import { parseInstant } from "./production-dispatch";
import {
  PlanAnswer,
  PlanRequest,
  type StandinIssue,
  StandinIssues,
  type StandinLogLine,
} from "./scored-runs-contracts";

const GRANT_TTL_MS = 60_000;

/** One recorded answer of an answers file. A member this does not name is refused, so an entry
 * the stand-in cannot judge never answers a request unconditionally. */
const AnswerEntry = z.strictObject({
  name: z.string().min(1),
  method: z.string().min(1),
  path: z.string().startsWith("/"),
  match: z.record(z.string(), z.json()).default({}),
  status: z.number().int().min(100).max(599),
  body: z.json().optional(),
});
export type AnswerEntry = z.infer<typeof AnswerEntry>;

/** `daemon-answers.json` (t4-contracts.md): what the daemon answered, captured by lane D's controls. */
const AnswersFile = z.strictObject({
  version: z.literal(1),
  captured: z.looseObject({ command: z.string(), at: z.string(), commit: z.string() }),
  answers: z.array(AnswerEntry),
});

/** The entries of the answers file at `file`, refused when it does not parse. */
export function readAnswers(file: string): AnswerEntry[] {
  const parsed = AnswersFile.safeParse(JSON.parse(readFileSync(file, "utf8")));
  if (!parsed.success)
    throw new Error(`${file} is no daemon-answers.json: ${parsed.error.message}`);
  return parsed.data.answers;
}

/** The value at the dot path `at` of `value`: object members by name, array members by index. */
function valueAt(value: unknown, at: string): unknown {
  let current = value;
  for (const step of at.split(".")) {
    if (Array.isArray(current)) current = current[Number(step)];
    else if (typeof current === "object" && current !== null) current = Reflect.get(current, step);
    else return undefined;
  }
  return current;
}

/** Whether `actual` is the JSON `expected`: equal, members and elements deeply. */
function sameJson(expected: unknown, actual: unknown): boolean {
  if (Array.isArray(expected)) {
    return (
      Array.isArray(actual) &&
      actual.length === expected.length &&
      expected.every((element, index) => sameJson(element, actual[index]))
    );
  }
  if (typeof expected === "object" && expected !== null) {
    if (typeof actual !== "object" || actual === null || Array.isArray(actual)) return false;
    const members = Object.entries(expected);
    return (
      members.length === Object.keys(actual).length &&
      members.every(([key, member]) => sameJson(member, Reflect.get(actual, key)))
    );
  }
  return expected === actual;
}

/** The first entry of `answers` that answers the request: its method and path, and each `match`
 * value at its dot path (`null` holds where the request has no value). */
export function recordedAnswer(
  answers: readonly AnswerEntry[],
  method: string,
  pathname: string,
  body: unknown
): AnswerEntry | undefined {
  return answers.find(
    (entry) =>
      entry.method === method &&
      entry.path === pathname &&
      Object.entries(entry.match).every(([at, expected]) => {
        const actual = valueAt(body, at);
        return expected === null
          ? actual === undefined || actual === null
          : sameJson(expected, actual);
      })
  );
}

/** `value` with the value of each `secret` and `bootToken` member replaced, at any depth. */
function redact(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(redact);
  if (typeof value !== "object" || value === null) return value;
  return Object.fromEntries(
    Object.entries(value).map(([key, member]) => [
      key,
      key === "secret" || key === "bootToken" ? "<redacted>" : redact(member),
    ])
  );
}

function stringField(body: unknown, key: string): string | undefined {
  if (typeof body !== "object" || body === null) return undefined;
  const value = Reflect.get(body, key);
  return typeof value === "string" ? value : undefined;
}

function numberField(body: unknown, key: string): number | undefined {
  if (typeof body !== "object" || body === null) return undefined;
  const value = Reflect.get(body, key);
  return typeof value === "number" ? value : undefined;
}

/** `nanos` since the epoch as Go's RFC3339Nano writes it in UTC: no trailing zero of a fraction. */
function rfc3339(nanos: bigint): string {
  const fraction = (nanos % 1_000_000_000n).toString().padStart(9, "0").replace(/0+$/, "");
  return new Date(Number(nanos / 1_000_000n))
    .toISOString()
    .replace(/\.\d{3}Z$/, fraction === "" ? "Z" : `.${fraction}Z`);
}

/** An answer: its status, and its JSON body where it has one, or `text`, a body sent as it stands
 * (the label's own answer to `register_plan`). `minted` is the grant a grant request minted, which
 * a scored run's log line names. */
interface Reply {
  readonly status: number;
  readonly body?: unknown;
  readonly text?: string;
  readonly headers?: Record<string, string>;
  readonly minted?: string;
}

const answered = (body: unknown = {}): Reply => ({ status: 200, body });
const json = (status: number, body: unknown): Reply => ({ status, body });
const forbidden = (message: string): Reply => json(403, { error: message });
/** A workflow refusal in the daemon's `Failure` shape, `{code, error}` (`writeFailure`). */
const failure = (status: number, code: string, message: string): Reply =>
  json(status, { code, error: message });

/** A grant the stand-in minted: when it lapses, and whether it is the controller's. */
interface Grant {
  readonly expiresAt: number;
  readonly controller: boolean;
}

/** A route's answer, or the refusal it gave before any answer was due. An answer of the stand-in's
 * own state (`own`) is never one an answers file holds. `took` is what an answer moves in that
 * state, whether a recorded answer or the route's own gave it. `logged` is what a scored run's log
 * line keeps of a request that parsed (its `body`). */
type Prepared = (
  | { readonly refusal: Reply }
  | {
      readonly answer: () => Reply | Promise<Reply>;
      readonly own?: boolean;
      readonly took?: (reply: Reply) => void;
    }
) & { readonly logged?: Record<string, unknown> };
type Route = (body: unknown) => Prepared;

/** `handoff/complete`'s body, held to the contract's shape and types and no further: the
 * contract's refinements (a look's report names no commit, a rating comes with its reason,
 * `returnTo` goes with a fail) are refusals of the daemon's each with a code of its own, which
 * `complete` makes in the daemon's order. */
const HandoffCompleteBody = z.strictObject(LegionHandoffCompleteRequest.shape);
type HandoffComplete = z.output<typeof HandoffCompleteBody>;

/** The role that works each phase, the daemon's `phase.RoleFor`
 * (`packages/daemon/internal/phase/phase.go`): a phase no role works (awaiting_person,
 * awaiting_children, done, held, unrecorded) has no row. A completion is held to it, and
 * `claims/owed` reads it. */
const ROLE_FOR_PHASE: Readonly<Record<string, LegionRole>> = {
  admitted: "architect",
  planning: "planner",
  implementing: "implementer",
  testing: "tester",
  reviewing: "reviewer",
  accepting: "architect",
  retro: "implementer",
  merging: "merger",
};

/** The response header the daemon states its own contract in
 * (`packages/daemon/internal/api/server.go`). */
const DAEMON_CONTRACT_HEADER = "Legion-Daemon-Contract";

/** The run's Dispatch, which a scored run's `register_plan` reads the plan from, and its bearer
 * token. */
interface RunDispatch {
  readonly url: string;
  readonly token: string;
}

export interface StandinOptions {
  readonly port: number;
  readonly logFile: string;
  /** The boot token `claims/register` is checked against. */
  readonly bootToken: string;
  readonly project: string;
  readonly issue: string;
  /** The root of the issue's tree: the issue itself for a root. */
  readonly tree: string;
  readonly role: LegionRole | "controller";
  readonly repository: string;
  readonly defaultBranch: string;
  /** The phase the issue stands in. */
  readonly phase: string;
  /** The word of the handoff record each phase ends with, none for a phase that ends with none
   * (daemon-pane.go `handoff-words`). */
  readonly handoffWords: Readonly<Record<string, string>>;
  readonly world?: World;
  /** The file of the launch's wall-clock instant (`--clock-real`, the case's clock-real.txt), which
   * the case's now moves on from. It needs `world`. Unset, the case's now moves on from the
   * stand-in's start. */
  readonly clockReal?: string;
  /** The recorded answers, the case's before the shared file's. */
  readonly answers?: readonly AnswerEntry[];
  /** The run's `calls.jsonl`, which says which pull requests the run opened and closed. */
  readonly ghCalls?: string;
  /** The contract the `Legion-Daemon-Contract` header states: this plugin build's own when unset. */
  readonly contract?: string;
  /** The stand-in of a scored run or of rig.sh's tester-proof run (the header): the world's issues
   * of `--issues`, and the label checkout (`--label`) and run's Dispatch (`--dispatch`) its
   * `register_plan` asks. Unset, the stand-in replays a case. */
  readonly scored?: {
    readonly issues: readonly StandinIssue[];
    readonly label?: string;
    readonly dispatch?: RunDispatch;
  };
}

export interface Standin {
  readonly port: number;
  stop(): void;
}

/** One line of the run's `calls.jsonl` (gh-standin.ts): its exit status, and the change it made
 * to a pull request, which gh-standin records only for a call it took. A line of another stand-in
 * (rig.sh's `bun`) names no change. */
const CallLine = z.looseObject({
  exit: z.number(),
  pull: z
    .looseObject({
      action: z.string(),
      number: z.number().optional(),
      head: z.string().optional(),
      auto: z.boolean().optional(),
    })
    .optional(),
});

/** Whether `legion/<issue>` has an open pull request: one the world holds open on the branch, then
 * each change gh-standin recorded in `calls`, in order: a pull request opened on the branch, and a
 * close, a merge that waits for nothing (no `auto`) and a reopen of one of the branch's. */
function openPullRequest(
  issue: string,
  world: World | undefined,
  calls: string | undefined
): boolean {
  const branch = `legion/${issue}`;
  const pulls = world?.github?.pullRequests.filter((pr) => pr.head.ref === branch) ?? [];
  const ours = new Set(pulls.map((pr) => pr.number));
  const open = new Set(pulls.filter((pr) => pr.state === "open").map((pr) => pr.number));
  if (calls === undefined || !existsSync(calls)) return open.size > 0;
  for (const line of readFileSync(calls, "utf8").split("\n")) {
    if (line === "") continue;
    const { exit, pull } = CallLine.parse(JSON.parse(line));
    const number = pull?.number;
    if (exit !== 0 || pull === undefined || number === undefined) continue;
    if (pull.action === "open" && pull.head === branch) {
      ours.add(number);
      open.add(number);
    } else if (!ours.has(number)) continue;
    else if (pull.action === "close" || (pull.action === "merge" && pull.auto !== true))
      open.delete(number);
    else if (pull.action === "reopen") open.add(number);
  }
  return open.size > 0;
}

/** The entry of the label's own code, under the label's checkout (C2). */
const ENTRY = path.join("packages", "pi-legion", "scripts", "skill-scenarios", "label-texts.sh");

/** What Dispatch's document read answers (`GET /api/v1/artifacts/{id}`), as far as the stand-in
 * reads it: the issue the document is on, null for a document on no issue, and its latest
 * version. */
const DispatchDocument = z.object({
  issue_key: z.string().nullable(),
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
  dispatch: RunDispatch,
  route: string,
  schema: z.ZodType<T>
): Promise<{ readonly data: T } | { readonly refusal: Reply; readonly missing: boolean }> {
  let response: Response;
  try {
    response = await fetch(`${dispatch.url}${route}`, {
      headers: { Accept: "application/json", Authorization: `Bearer ${dispatch.token}` },
    });
  } catch (error) {
    return {
      refusal: failure(502, "DISPATCH_FAILED", `the read of GET ${route} failed: ${error}`),
      missing: false,
    };
  }
  const text = await response.text();
  let parsed: ZodSafeParseResult<T> | undefined;
  try {
    parsed = response.ok ? schema.safeParse(JSON.parse(text)) : undefined;
  } catch {
    parsed = undefined;
  }
  if (parsed === undefined || !parsed.success) {
    return {
      refusal: failure(
        502,
        "DISPATCH_FAILED",
        `the run's Dispatch answered GET ${route} with ${response.status} and a body the stand-in cannot read: ${text}`
      ),
      missing: response.status === 404,
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

/** A scored run's `register_plan` answer: the version from the run's Dispatch, given to the
 * label's own route through the entry, once the document is the request's issue's
 * (documentOfIssue's rule). */
async function planCheck(
  label: string | undefined,
  dispatch: RunDispatch | undefined,
  request: { readonly issue: string; readonly artifactId: string; readonly version: number }
): Promise<Reply> {
  if (label === undefined || dispatch === undefined) {
    const missing = [
      label === undefined ? "--label" : "",
      dispatch === undefined ? "--dispatch" : "",
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
  const document = await dispatchRead(dispatch, documentRoute, DispatchDocument);
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
    const version = await dispatchRead(dispatch, route, DispatchVersion);
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
    label,
    PlanRequest.parse({ artifactId, version: request.version, latestVersion, blocks })
  );
}

/** The statuses whose response has no body (the Fetch standard's null body statuses that a
 *  response can be made with). */
const BODILESS_STATUSES: ReadonlySet<number> = new Set([204, 205, 304]);

/** The entry's answer to one plan request: its status and its body, or the 500 naming its exit and
 * the last line of its stderr. Its output goes to this process's stderr, the rig's log of it. */
async function runEntry(label: string, request: PlanRequest): Promise<Reply> {
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
    const headers = { "Content-Type": "application/json" };
    return answer.body === ""
      ? { status: answer.status, headers }
      : { status: answer.status, text: answer.body, headers };
  } finally {
    rmSync(scratch, { recursive: true, force: true });
  }
}

/** Starts the stand-in daemon for one run. */
export function startDaemonStandin(options: StandinOptions): Standin {
  const { project, issue, tree, role, repository, defaultBranch, world, clockReal, scored } =
    options;
  const answers = options.answers ?? [];
  const startedAt = new Date().toISOString();
  const startedNs = BigInt(Date.now()) * 1_000_000n;
  /** The case's clock at the instant it starts to move on from: the world's, else the stand-in's
   * start. */
  const clockAt = (world === undefined ? undefined : parseInstant(world.clock)) ?? startedNs;
  if (clockReal !== undefined && world === undefined) {
    throw new Error(
      `--clock-real ${clockReal} names the launch instant a case's clock moves on from, and this start has no case world (--world)`
    );
  }
  /** The case's clock now, or why it is unknown. With `clockReal`, it is the world's clock moved on
   * by the time since the launch instant that file holds, as outcome.ts and lane D's replay put
   * each record of the run on the case's timeline (clock.txt plus the time since clock-real.txt).
   * rig.sh writes that file after the stand-in starts, so the file is read at each call, and the
   * stand-in's start never takes its place. Without `clockReal`, the time since the stand-in
   * started moves the clock on. */
  const caseNow = (): bigint | string => {
    const now = BigInt(Date.now()) * 1_000_000n;
    if (clockReal === undefined) return clockAt + now - startedNs;
    let text: string;
    try {
      text = readFileSync(clockReal, "utf8").trim();
    } catch (error) {
      return `the launch instant file ${clockReal} cannot be read: ${error instanceof Error ? error.message : String(error)}`;
    }
    const launched = parseInstant(text);
    if (launched === undefined) {
      return `the launch instant file ${clockReal} holds ${JSON.stringify(text)}, which is no RFC 3339 time`;
    }
    return clockAt + now - launched;
  };
  /** Encoded exactly as the daemon encodes it (`legion-<project>-<key>-<role>`, lower-cased),
   * since the worker claims this token on the real Envoy and Envoy rejects uppercase. */
  const roleTokenValue =
    role === "controller" ? controllerToken(project) : roleToken(project, issue, role);
  /** The worker's registration secret: random, as the daemon's is, since the pane holds it and
   * scrubs it from every line it prints (`@legion/contracts/held-secrets`). */
  const sessionSecret = fixtureSecret("reg");
  const grants = new Map<string, Grant>();

  /** The world's issues of a scored run (`--issues`): the claim's own entry, and each other issue
   * in a phase, which the daemon records beside the claim's. */
  const claimInWorld = scored?.issues.find((each) => each.key === issue);
  const worldIssues =
    scored?.issues.flatMap((each) =>
      each.key === issue || each.phase === null ? [] : [{ ...each, phase: each.phase }]
    ) ?? [];
  /** The issues of the claim's tree: the claim's own, and each issue of the world in that tree. */
  const treeIssues: ReadonlySet<string> = new Set([
    issue,
    ...(scored?.issues ?? []).filter((each) => each.tree === tree).map((each) => each.key),
  ]);

  const daemonWorld = world?.daemon ?? null;
  /** What moves while the run goes on: the phase a backward move sets, the status `set_status`
   * writes, the phase the claim's accepted completion was of (until the phase is set again),
   * whether the production check is still open, whether its look is, when its next look is due,
   * and the looks reported on it. */
  const held = {
    phase: options.phase,
    status: world?.dispatch.issue.status ?? claimInWorld?.status ?? "in_progress",
    completed: undefined as string | undefined,
    checkOpen: daemonWorld?.productionCheck != null,
    lookOpen: true,
    nextWakeAt: daemonWorld?.productionCheck?.boundAt ?? "",
    reported: new Set<string>(),
  };

  /** The open production check as `GET .../record` shows it (`api.CheckView`): the look the role
   * works, open until a report ends it, and the next look, due at the time a pending report named
   * or else at the check's bound, as the daemon's is. */
  const checkView = () => {
    const check = daemonWorld?.productionCheck;
    if (check === null || check === undefined || !held.checkOpen) return undefined;
    const pulls = world?.github?.pullRequests ?? [];
    const merged = pulls.find((pr) => pr.mergeCommit === check.mergeCommit) ?? pulls[0];
    return {
      repo: world?.github?.repository ?? repository,
      pullRequest: merged?.number ?? 1,
      mergeCommit: check.mergeCommit,
      openedAt: check.openedAt,
      role: "tester",
      looks: check.look,
      lookOpen: held.lookOpen,
      lastWake: {
        kind: check.last ? "bound" : check.look === 1 ? "merge" : "check wake",
        ...(check.look === 1 ? { at: check.openedAt } : {}),
      },
      nextWakeAt: held.nextWakeAt,
      last: check.last,
    };
  };

  /** The one issue the role holds, in the phase the world stands it in, on the repository and
   * default branch the rig names. */
  const issueRecord = () => {
    const pull = world?.github?.pullRequests.find((pr) => pr.head.ref === `legion/${issue}`);
    const check = held.phase === "done" ? checkView() : undefined;
    return {
      key: issue,
      generation: 1,
      phase: held.phase,
      status: held.status,
      tree,
      repo: repository,
      defaultBranch,
      workers: {},
      ...(pull === undefined
        ? {}
        : {
            pullRequest: {
              repo: repository,
              number: pull.number,
              head: pull.head.sha,
              state: pull.state,
            },
          }),
      ...(check === undefined ? {} : { check }),
    };
  };

  /** The issues the daemon records: the claim's (`issueRecord`), then each issue of the world in a
   * phase, in its tree, on the repository and default branch the rig names. */
  const recorded = () => [
    issueRecord(),
    ...worldIssues.map((each) => ({
      key: each.key,
      generation: 1,
      phase: each.phase,
      status: each.status,
      tree: each.tree,
      repo: repository,
      defaultBranch,
      workers: {},
    })),
  ];

  /** `GET /legion/v1/state`: the issues the daemon records (`recorded`), each live unless it
   * stands in a phase no worker holds a slot in. `capabilities` is the deployment's capability
   * report, `strandedClaims` the claims nothing will wake, `heldOutboxRows` the lifecycle rows held
   * behind their issues' running ones, `consumers` the lag of the daemon's durable consumers and
   * `repositories` the repositories the live records name, each of which the reader requires; the
   * rig has no deployment to report on, its one claim runs, it has no outbox and no NATS, so the
   * first four are the empty lists the daemon's MarshalJSON emits when there is none.
   * `wakesWaiting`, the wakes the pacing holds until their turn, is optional to the reader; the rig
   * carries it empty, as a daemon at this contract serves a caller in no pod holding no wake.
   * `settings` is the daemon's resolved legion.yaml with no credential in it, also required; the
   * rig has no file, so it carries the two keys the reader types at the daemon's defaults. */
  const stateDocument = () => {
    const issues = recorded();
    const live = issues.filter((each) => !["done", "held", "awaiting_person"].includes(each.phase));
    return {
      daemon: { project, schemaVersion: 1, boots: 1, firstBootAt: startedAt, startedAt },
      admission: {
        cap: issues.length,
        active: live.map((each) => each.key),
        waiting: [],
        trees: [
          ...new Set(issues.filter((each) => each.phase !== "done").map((each) => each.tree)),
        ],
        blocked: [],
      },
      issues: Object.fromEntries(issues.map((each) => [each.key, each])),
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
    };
  };
  // A state these options cannot make is refused at the start, not at the first read of it.
  const opening = LegionStateResponse.safeParse(stateDocument());
  if (!opening.success) {
    throw new Error(`the state document this stand-in makes is no state: ${opening.error.message}`);
  }

  /** `GET /healthz`: that this stand-in is serving, and the project its state names, which is the
   * one word the controller's boot check reads. The contract goes in the header beside it, where
   * the tool's `read_build` reads it: the label's (`contract`), else this plugin build's own
   * (`daemonContract()`), which is the contract a daemon this build pairs with speaks. The
   * stand-in runs from a checkout, whose manifest declares none, so it is the frozen list's
   * greatest number plus the count of entry fixtures, the number the pack writes into the manifest
   * it packs. */
  const health = (): Reply => ({
    status: 200,
    body: LegionHealthResponse.parse({ status: "ok", project }),
    headers: {
      [DAEMON_CONTRACT_HEADER]: options.contract ?? String(daemonContract(pkg, import.meta.dir)),
    },
  });

  /** A route: its request is parsed with `schema`, named `name` in the 400 that refuses a body
   * that does not parse, held to a live grant when `grantOf` names the request's, and then
   * answered by `handle`, unless an answers file answers it first; a request `own` names is
   * answered by `handle` alone. `took` is what the answer, either one, moves in the stand-in's
   * state. `logged` is what a scored run's log line keeps of the request. */
  function route<S extends z.ZodType>(
    name: string,
    schema: S,
    handle: (request: z.output<S>, grant: Grant | undefined) => Reply | Promise<Reply>,
    how: {
      readonly grantOf?: (request: z.output<S>) => string;
      readonly own?: (request: z.output<S>) => boolean;
      readonly took?: (request: z.output<S>, reply: Reply) => void;
      readonly logged?: (request: z.output<S>) => Record<string, unknown>;
    } = {}
  ): Route {
    return (body) => {
      const parsed = schema.safeParse(body);
      if (!parsed.success) {
        return { refusal: json(400, { error: `the body is no ${name}: ${parsed.error.message}` }) };
      }
      const logged = how.logged?.(parsed.data);
      let grant: Grant | undefined;
      if (how.grantOf !== undefined) {
        grant = grants.get(how.grantOf(parsed.data));
        if (!grant || grant.expiresAt <= Date.now()) {
          return { refusal: forbidden("Invalid or expired grant"), logged };
        }
      }
      return {
        answer: () => handle(parsed.data, grant),
        own: how.own?.(parsed.data) ?? false,
        took: (reply) => how.took?.(parsed.data, reply),
        logged,
      };
    };
  }
  const grantId = (request: { grantId: string }) => request.grantId;
  /** An owner's route: its grant, and its whole parsed body in a scored run's log line. */
  const owner = { grantOf: grantId, logged: (request: Record<string, unknown>) => request };

  /** Whether the worker of this claim is the tree root's architect: the design gate is its alone. */
  const isRoot = issue === tree;

  /** `handoff/complete` for a production check's look report: the daemon's `checkReport`, against
   * the check the world holds open, and the state the daemon's workflow leaves (workflow's
   * `checkReported`): a pass or a fail closes the check, and so does a pending on its last look
   * (`endLookPending`: it closes as not run); a pending on any other look ends the look, and the
   * next look is due at the time it names, as `clampWake` moves it: a minute after the case's now
   * at the earliest (`CheckWakeFloor`), the bound where it falls at or past the bound, to the
   * microsecond. */
  const checkReport = (request: HandoffComplete): Reply => {
    const check = daemonWorld?.productionCheck;
    const opened = request.checkOpenedAt ?? "";
    if (check === null || check === undefined || !held.checkOpen) {
      return failure(
        409,
        "CHECK_NOT_OPEN",
        `${issue} has no open production check opened at ${opened}; this completion changed nothing`
      );
    }
    // The daemon compares the two instants to the nanosecond, in any offset
    // (`check.OpenedAt.Equal`), and a JavaScript Date reads milliseconds alone.
    const openedAt = parseInstant(opened);
    if (openedAt === undefined || openedAt !== parseInstant(check.openedAt)) {
      return failure(
        409,
        "CHECK_NOT_OPEN",
        `${issue}'s production check is open, but it did not open at ${opened}. This completion changed nothing. Use check_opened_at from its look task or check.openedAt from read_record as checkOpenedAt. Send the report again with that time.`
      );
    }
    if (role !== "tester") {
      return failure(
        409,
        "CHECK_ROLE_MISMATCH",
        `${issue}'s production check is the tester's, and this completion is the ${role}'s: only the role the check records reports on it; the record is unchanged, so the check's own role may send this report`
      );
    }
    const verdict = request.verdict;
    if (verdict !== "pass" && verdict !== "pending" && verdict !== "fail") {
      return failure(
        400,
        "CHECK_VERDICT_REQUIRED",
        "a production check report names a verdict of pass, pending or fail"
      );
    }
    if (verdict === "pending" && request.wakeAt === undefined && !check.last) {
      return failure(
        400,
        "CHECK_WAKE_REQUIRED",
        `a pending report names wakeAt, when the next look of ${issue}'s production check is due; look ${check.look} stays open and nothing was recorded, so send this report again with a time`
      );
    }
    if (request.wakeAt !== undefined && (verdict !== "pending" || check.last)) {
      const why =
        verdict === "pending"
          ? `look ${check.look} is the check's last, after which it closes as not run`
          : `a ${verdict} report closes the check`;
      return failure(
        400,
        "CHECK_WAKE_UNEXPECTED",
        `this report names wakeAt and no look follows it: ${why}; send it again without a time, and nothing was recorded`
      );
    }
    if (
      request.commit !== undefined ||
      request.handoffId !== undefined ||
      request.handoffVersion !== undefined
    ) {
      return failure(
        400,
        "CHECK_PHASE_FIELDS_UNEXPECTED",
        "a check report carries no commit, handoffId or handoffVersion. Remove those phase fields and send the report again. Nothing was recorded."
      );
    }
    const what = `this report on look ${check.look} of ${issue}'s production check`;
    if (request.planRating !== undefined || request.planReason !== undefined) {
      return failure(
        400,
        "PLAN_RATING_UNEXPECTED",
        `${what} names planRating or planReason, and only the completion that ends the retro rates the plan; send it again without both, and nothing was recorded`
      );
    }
    if (request.returnTo !== undefined) {
      return failure(
        400,
        "RETURN_TO_NEEDS_FAIL",
        `${what} names returnTo, and only the completion of a step that failed, with the verdict fail, names the step its task returns to: send it again without returnTo; nothing was recorded`
      );
    }
    const reported = `${check.look}:${verdict}`;
    if (held.reported.has(reported)) {
      return failure(
        409,
        "HANDOFF_ALREADY_RECORDED",
        `the ${verdict} report on look ${check.look} of ${issue}'s production check was already received; this call changed nothing`
      );
    }
    // The case's now is read before the report moves anything, so a launch instant the stand-in
    // cannot read leaves the check as it stood.
    const now = verdict !== "pending" || check.last ? undefined : caseNow();
    if (typeof now === "string") {
      const reason = `${now}. The case's now is unknown, so this report changed nothing.`;
      console.error(`daemon-standin: handoff/complete answered 500: ${reason}`);
      return failure(500, "CASE_CLOCK_UNREADABLE", reason);
    }
    held.reported.add(reported);
    if (now === undefined) held.checkOpen = false;
    else {
      held.lookOpen = false;
      const bound = parseInstant(check.boundAt) ?? 0n;
      const floor = now + 60_000_000_000n;
      const named = parseInstant(request.wakeAt ?? "") ?? floor;
      const wake = named < floor ? floor : named;
      held.nextWakeAt = wake >= bound ? check.boundAt : rfc3339(wake - (wake % 1000n));
    }
    return answered();
  };

  /** `handoff/complete`: the daemon's refusals in the order its route makes them (the header
   * lists them), then `{}`. */
  const complete = (request: HandoffComplete, grant: Grant | undefined): Reply => {
    if (grant?.controller) return failure(403, "GRANT_INVALID", "grant is unavailable");
    if (request.checkOpenedAt !== undefined) return checkReport(request);
    if (held.phase === "done") {
      const whose = held.checkOpen
        ? "its production check is the tester's; that role reports pass, pending or fail with checkOpenedAt, the check_opened_at time in its look task or check.openedAt from read_record"
        : "it has no open production check, so there is nothing here for any role to report";
      return failure(
        409,
        "CHECK_LOOK_NOT_SERVED",
        `${issue} is done: no phase of it is open, and this completion names no check with checkOpenedAt. This completion reported on nothing and recorded nothing: ${whose}.`
      );
    }
    if ((role === "implementer" || role === "merger") && request.commit === undefined) {
      return failure(400, "MISSING_FIELD", "commit is required");
    }
    if (role === "tester" && request.verdict !== "pass" && request.verdict !== "fail") {
      return failure(400, "TESTER_VERDICT_REQUIRED", "tester verdict must be pass or fail");
    }
    if (role !== "tester" && request.verdict !== "") {
      return failure(400, "VERDICT_ROLE_FORBIDDEN", "only a tester may report a verdict");
    }
    if (request.returnTo !== undefined && request.verdict !== "fail") {
      return failure(
        400,
        "RETURN_TO_NEEDS_FAIL",
        `this completion of phase ${held.phase} of ${issue} names returnTo, and only the completion of a step that failed, with the verdict fail, names the step its task returns to: send it again without returnTo; nothing was recorded`
      );
    }
    if ((request.planRating === undefined) !== (request.planReason === undefined)) {
      return failure(
        400,
        "PLAN_RATING_INPUT",
        "planRating and planReason appear together: send the rating of the plan with its reason, or neither; nothing was recorded"
      );
    }
    if (request.wakeAt !== undefined) {
      return failure(
        400,
        "CHECK_WAKE_UNEXPECTED",
        `this completion of phase ${held.phase} of ${issue} names wakeAt, which belongs to the pending report of a production check's look and to nothing else; send it again without a time, and nothing was recorded`
      );
    }
    if (role === "architect") {
      return failure(
        409,
        "ACCEPTANCE_IS_SIGN_OFF",
        `the architect accepts ${issue} with sign_off, not handoff_complete; this completion changed nothing`
      );
    }
    if (ROLE_FOR_PHASE[held.phase] !== role) {
      return failure(
        409,
        "HANDOFF_NOT_CURRENT_PHASE",
        `the ${role} does not run phase ${held.phase} of ${issue}; this completion changed nothing`
      );
    }
    const word = options.handoffWords[held.phase] ?? "";
    const { handoffId, handoffVersion } = request;
    if (word !== "" && (handoffId === undefined || handoffVersion === undefined)) {
      const write = `\`dispatch handoff-write --issue ${issue} --phase ${word} --body-file <file>\``;
      return failure(
        400,
        "HANDOFF_RECORD_REQUIRED",
        `phase ${held.phase} of ${issue} ends with its ${word} handoff recorded on the Dispatch issue, and this completion names no record (handoffId and a positive handoffVersion): write it with ${write}, then complete again; this completion changed nothing`
      );
    }
    if (word === "" && (handoffId !== undefined || handoffVersion !== undefined)) {
      return failure(
        409,
        "HANDOFF_RECORD_MISMATCH",
        `phase ${held.phase} of ${issue} ends with no handoff record, and this completion names one: complete without handoffId and handoffVersion; this completion changed nothing`
      );
    }
    const step = daemonWorld?.step;
    if (request.returnTo !== undefined && step !== null && step !== undefined) {
      if (request.returnTo >= step.place) {
        return failure(
          409,
          "RETURN_REFUSED",
          `returnTo is ${request.returnTo}, and step ${request.returnTo} of ${issue}'s task is not before step ${step.place} (${step.name}), the step that failed; send the fail again naming an earlier step, or without returnTo; nothing was recorded`
        );
      }
    }
    if (
      role === "implementer" &&
      held.phase === "implementing" &&
      !openPullRequest(issue, world, options.ghCalls)
    ) {
      return failure(
        409,
        "HANDOFF_NO_PULL_REQUEST",
        `no open pull request on ${repository} legion/${issue}; nothing was recorded. Open the pull request and complete again.`
      );
    }
    return answered();
  };

  /** `POST /legion/v1/claims/owed`: the daemon's `owedBy` (`api/owed.go`), read from the record
   * this stand-in holds (`issueRecord`). The architect and the controller owe nothing and name no
   * architect. A phase worker owes the production check's look while the record holds the check
   * and its look open for the worker's role; else it owes the phase the record holds its issue in
   * while its role works that phase (`ROLE_FOR_PHASE`) and the stand-in has accepted no completion
   * of it since the phase was set (`held.completed`), and a merger whose pull request the world
   * holds merged owes nothing, since that merge finishes merging. Each phase worker's answer names
   * the architect that owns the claim, the end of the daemon's owner walk: no issue the stand-in
   * records holds an architect claim, so the walk ends at the tree root's architect. */
  const owedAnswer = (): LegionOwed => {
    if (role === "architect" || role === "controller") {
      return LegionOwedResponse.parse({ owes: false, work: "" });
    }
    const record = issueRecord();
    let work = "";
    if (record.check?.lookOpen === true && record.check.role === role) {
      work = `the production check of ${issue}`;
    } else if (
      ROLE_FOR_PHASE[held.phase] === role &&
      held.completed !== held.phase &&
      !(held.phase === "merging" && record.pullRequest?.state === "merged")
    ) {
      work = `the ${held.phase} phase of ${issue}`;
    }
    return LegionOwedResponse.parse({
      owes: work !== "",
      work,
      architect: roleToken(project, tree, "architect"),
    });
  };

  const routes: Record<string, Route> = {
    "POST /legion/v1/claims/register": (body) => ({
      answer: () => {
        if (stringField(body, "bootToken") !== options.bootToken) {
          return forbidden("Invalid boot token");
        }
        return answered(
          role === "controller"
            ? { claimToken: roleTokenValue, role, generation: 1, secret: sessionSecret }
            : {
                claimToken: roleTokenValue,
                tree,
                issue,
                role,
                generation: 1,
                secret: sessionSecret,
                promptAgents: [],
              }
        );
      },
    }),
    "POST /legion/v1/claims/ready": route("LegionReadyRequest", LegionReadyRequest, (request) =>
      request.secret === sessionSecret ? { status: 204 } : forbidden("Invalid session secret")
    ),
    // The pane's turn-boundary reports, which the daemon takes with the claim's secret: activity
    // answers 204 with no body, as `acknowledge` reads it, and a turn's outcome answers the run of
    // refused turns its reports end with (`claim.TurnOutcomeResponse`), none here, failing nothing.
    // A scored run's line of an activity report keeps the report's activity alone.
    "POST /legion/v1/claims/activity": route(
      "LegionActivityRequest",
      LegionActivityRequest,
      (request) =>
        request.secret === sessionSecret ? { status: 204 } : forbidden("Invalid session secret"),
      { logged: (request) => request.activity }
    ),
    "POST /legion/v1/claims/turn-outcome": route(
      "LegionTurnOutcomeRequest",
      LegionTurnOutcomeRequest,
      (request) =>
        "secret" in request && request.secret !== sessionSecret
          ? forbidden("Invalid session secret")
          : answered({ refusedRun: 0, failed: false })
    ),
    // What the claim owes, under its secret: the phase reminder asks after each turn, and the
    // stall watch asks for its notice's owner.
    "POST /legion/v1/claims/owed": route("LegionOwedRequest", LegionOwedRequest, (request) =>
      request.secret === sessionSecret
        ? answered(owedAnswer())
        : forbidden("Invalid session secret")
    ),
    "POST /legion/v1/grants": (body) => {
      const controller = role === "controller";
      const parsed = (controller ? LegionControllerGrantRequest : LegionGrantRequest).safeParse(
        body
      );
      if (!parsed.success) {
        const schema = controller ? "LegionControllerGrantRequest" : "LegionGrantRequest";
        return {
          refusal: json(400, { error: `the body is no ${schema}: ${parsed.error.message}` }),
        };
      }
      return {
        answer: () => {
          if (parsed.data.secret !== sessionSecret) return forbidden("Invalid session secret");
          const id = randomUUID();
          const expiresAt = Date.now() + GRANT_TTL_MS;
          grants.set(id, { expiresAt, controller });
          return {
            ...answered({ grantId: id, expiresAt: new Date(expiresAt).toISOString() }),
            minted: id,
          };
        },
      };
    },
    // A look report is answered from the check this stand-in holds, whose state each report
    // moves: an answer recorded against one state of a check is no answer to another. A step's
    // completion that is accepted, by `complete` or by a recorded answer, is the claim's completion
    // of the phase its issue stands in, which `claims/owed` then answers it owes nothing of.
    "POST /legion/v1/handoff/complete": route(
      "LegionHandoffCompleteRequest",
      HandoffCompleteBody,
      complete,
      {
        grantOf: grantId,
        own: (request) => request.checkOpenedAt !== undefined,
        took: (request, reply) => {
          if (request.checkOpenedAt === undefined && reply.status >= 200 && reply.status < 300) {
            held.completed = held.phase;
          }
        },
      }
    ),
    "POST /legion/v1/phase/backward": route(
      "LegionPhaseBackwardRequest",
      LegionPhaseBackwardRequest,
      (request) => {
        if (role !== "architect" && request.issue !== undefined && request.issue !== issue) {
          return failure(
            400,
            "ISSUE_NOT_GRANTS",
            `a phase worker moves only its own issue ${issue}, not ${request.issue}`
          );
        }
        if (request.issue === undefined || request.issue === issue) {
          held.phase = request.to;
          held.completed = undefined;
        }
        return answered();
      },
      owner
    ),
    "POST /legion/v1/gates/register": route(
      "LegionGateRegisterRequest",
      LegionGateRegisterRequest,
      () =>
        isRoot
          ? answered()
          : failure(
              403,
              "GATE_ROOT_ONLY",
              `the design gate belongs to the tree root ${tree}; register it there`
            ),
      owner
    ),
    // A scored run's register_plan is the label's own route, in its order: the grant (route), the
    // issue's tree (planOwner), then the document's issue (planCheck).
    "POST /legion/v1/plans/register": route(
      "LegionPlanRegisterRequest",
      LegionPlanRegisterRequest,
      (request) => {
        if (scored === undefined) return answered({ plan: { version: request.version } });
        if (!treeIssues.has(request.issue)) {
          return failure(
            403,
            "ISSUE_OUTSIDE_TREE",
            `${request.issue} is outside the architect tree ${tree}`
          );
        }
        return planCheck(scored.label, scored.dispatch, request);
      },
      owner
    ),
    "POST /legion/v1/signoff": route(
      "LegionSignOffRequest",
      LegionSignOffRequest,
      () => answered(),
      owner
    ),
    "POST /legion/v1/waves/release": route(
      "LegionWaveReleaseRequest",
      LegionWaveReleaseRequest,
      (request) => answered(scored === undefined ? { released: [...new Set(request.issues)] } : {}),
      owner
    ),
    "POST /legion/v1/children/rerun": route(
      "LegionChildRequest",
      LegionChildRequest,
      () => answered(),
      owner
    ),
    "POST /legion/v1/tasks/stop": route(
      "LegionTaskStopRequest",
      LegionTaskStopRequest,
      () => answered(),
      owner
    ),
    "POST /legion/v1/issues/status": route(
      "LegionIssueStatusRequest",
      LegionIssueStatusRequest,
      (request, grant) => {
        if (!grant?.controller) {
          return failure(403, "CONTROLLER_REQUIRED", "issue status requires a controller grant");
        }
        if (request.issue === issue) held.status = request.status;
        return answered();
      },
      { grantOf: grantId }
    ),
    "GET /healthz": () => ({ answer: health }),
    "GET /legion/v1/state": () => ({
      answer: () => answered(LegionStateResponse.parse(stateDocument())),
    }),
  };

  /** `GET /legion/v1/issues/<key>/record`, and the key it names. */
  const RECORD_ROUTE = /^\/legion\/v1\/issues\/([^/]+)\/record$/;
  const resolve = (method: string, pathname: string): Route | undefined => {
    const exact = routes[`${method} ${pathname}`];
    if (exact !== undefined || method !== "GET") return exact;
    const record = RECORD_ROUTE.exec(pathname);
    if (record === null) return undefined;
    return () => ({
      answer: () => {
        const key = decodeURIComponent(record[1] ?? "");
        const found = recorded().find((each) => each.key === key);
        return found === undefined
          ? failure(404, "RECORD_NOT_FOUND", `The daemon has no record for ${record[1]}`)
          : answered(LegionIssueRecordResponse.parse(found));
      },
    });
  };

  /** The answer to one request: the route's refusal where the request does not parse or holds no
   * live grant, else a recorded answer, unless the route answers it from the stand-in's own state,
   * else the route's own, then what that answer moves (`took`); and what a scored run's line keeps
   * of the request. */
  const respond = async (
    method: string,
    pathname: string,
    body: unknown
  ): Promise<{ readonly reply: Reply; readonly logged?: Record<string, unknown> }> => {
    const prepared = resolve(method, pathname)?.(body);
    const logged = prepared?.logged;
    if (prepared !== undefined && "refusal" in prepared) return { reply: prepared.refusal, logged };
    const recordedReply = prepared?.own
      ? undefined
      : recordedAnswer(answers, method, pathname, body);
    let reply: Reply;
    if (recordedReply !== undefined) {
      reply =
        recordedReply.status === 204
          ? { status: 204 }
          : { status: recordedReply.status, body: recordedReply.body ?? {} };
    } else if (prepared === undefined) {
      reply = json(404, { error: `no route for ${method} ${pathname}` });
    } else {
      reply = await prepared.answer();
    }
    prepared?.took?.(reply);
    return { reply, logged };
  };

  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: options.port,
    // A scored run's register_plan waits on the label's entry, which runs a test binary, for
    // longer than Bun's default of ten idle seconds.
    idleTimeout: 0,
    async fetch(request) {
      const pathname = new URL(request.url).pathname;
      let body: unknown;
      if (request.method === "POST") {
        try {
          body = await request.json();
        } catch {
          body = undefined;
        }
      }
      const { reply, logged } = await respond(request.method, pathname, body);
      const at = new Date().toISOString();
      const grantId = stringField(body, "grantId");
      const sessionId = stringField(body, "sessionId");
      const handoffId = stringField(body, "handoffId");
      const handoffVersion = numberField(body, "handoffVersion");
      const named = {
        ...(grantId === undefined ? {} : { grantId }),
        ...(sessionId === undefined ? {} : { sessionId }),
      };
      const record = {
        ...(handoffId === undefined ? {} : { handoffId }),
        ...(handoffVersion === undefined ? {} : { handoffVersion }),
      };
      // A case's line: the request and its answer whole, secrets redacted. A scored run's: C3's
      // StandinLogLine.
      const line =
        scored === undefined
          ? {
              at,
              method: request.method,
              path: pathname,
              status: reply.status,
              body: body === undefined ? null : redact(body),
              answer: reply.body === undefined ? null : redact(reply.body),
              ...named,
              ...record,
            }
          : ({
              at,
              path: pathname,
              status: reply.status,
              ...named,
              ...(reply.minted === undefined ? {} : { mintedGrantId: reply.minted }),
              ...record,
              ...(logged === undefined ? {} : { body: logged }),
            } satisfies StandinLogLine);
      appendFileSync(options.logFile, `${JSON.stringify(line)}\n`);
      if (reply.text !== undefined) {
        return new Response(reply.text, { status: reply.status, headers: reply.headers });
      }
      return reply.body === undefined
        ? new Response(null, { status: reply.status, headers: reply.headers })
        : Response.json(reply.body, { status: reply.status, headers: reply.headers });
    },
  });
  return { port: server.port ?? options.port, stop: () => server.stop(true) };
}

const USAGE =
  "usage: bun daemon-standin.ts <port> <log file> <boot token file> <project> <issue> <role> <repository> <default branch> [<phase>] [--tree <root>] [--world <file>] [--clock-real <file>] [--answers <file>] [--case-answers <file>] [--gh-calls <file>] [--issues <file>] [--label <checkout>] [--dispatch <url> <token file>]";

function refuse(message: string): never {
  console.error(message);
  process.exit(2);
}

/** The label's plugin as far as the stand-in reads it: the contract its manifest states, which a
 *  manifest in a checkout leaves out from the daemon API contract cutover on. */
const LabelPlugin = z.object({
  legion: z.object({ daemonApiVersion: z.number().int().positive().optional() }).optional(),
});

/** The contract the label's plugin speaks: `daemonContract` of that plugin's manifest from its own
 * directory, the manifest's `legion.daemonApiVersion` where it states one, else the frozen list's
 * greatest number plus the count of entry fixtures of the checkout that holds it. That is the
 * number the label's install writes into the plugin it packs. */
function labelContract(checkout: string): number {
  const plugin = path.join(checkout, "packages", "pi-legion");
  const file = path.join(plugin, "package.json");
  try {
    return daemonContract(LabelPlugin.parse(JSON.parse(readFileSync(file, "utf8"))), plugin);
  } catch (error) {
    refuse(`--label ${checkout}: its plugin states no contract (${file}): ${error}`);
  }
}

/** The run's Dispatch of `--dispatch`, and its bearer token read from `tokenFile`. */
function dispatchOf(url: string, tokenFile: string): RunDispatch {
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

/** The world's issues of `--issues`. */
function issuesOf(file: string): StandinIssue[] {
  try {
    return StandinIssues.parse(JSON.parse(readFileSync(file, "utf8")));
  } catch (error) {
    refuse(`--issues ${file}: ${error}`);
  }
}

if (import.meta.main) {
  // The command line: eight or nine words, then the options.
  const words = Bun.argv.slice(2);
  const firstOption = words.findIndex((word) => word.startsWith("--"));
  const positional = firstOption < 0 ? words : words.slice(0, firstOption);
  if (positional.length < 8 || positional.length > 9) refuse(USAGE);
  const [portArg, logFile, bootTokenFile, project, issue, role, repository, defaultBranch] =
    positional;
  /** The ninth word: the phase of a role that works none, absent for one that works a phase. */
  const phaseArg: string | undefined = positional[8];
  const values: {
    tree?: string;
    world?: string;
    clockReal?: string;
    answers?: string;
    caseAnswers?: string;
    ghCalls?: string;
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
        values.tree = take(option);
        break;
      case "--world":
        values.world = take(option);
        break;
      case "--clock-real":
        values.clockReal = take(option);
        break;
      case "--answers":
        values.answers = take(option);
        break;
      case "--case-answers":
        values.caseAnswers = take(option);
        break;
      case "--gh-calls":
        values.ghCalls = take(option);
        break;
      case "--issues":
        values.issues = take(option);
        break;
      case "--label":
        values.label = take(option);
        break;
      case "--dispatch":
        values.dispatchUrl = take(option);
        values.dispatchTokenFile = take(option);
        break;
      default:
        refuse(`${option} is no option of the stand-in; ${USAGE}`);
    }
  }
  if (role !== "controller" && !isLegionRole(role)) refuse(`${role} is not a Legion role`);
  if (phaseArg !== undefined && !LEGION_PHASES.some((phase) => phase === phaseArg)) {
    refuse(`${phaseArg} is not a phase of the daemon's (${LEGION_PHASES.join(", ")})`);
  }
  if (
    values.world !== undefined &&
    (values.issues !== undefined || values.label !== undefined || values.dispatchUrl !== undefined)
  ) {
    refuse(`--issues, --label and --dispatch are a scored run's, and --world a case's; ${USAGE}`);
  }
  const bootToken = (await Bun.file(bootTokenFile).text()).trim();
  if (bootToken.length === 0) refuse(`boot token file ${bootTokenFile} is empty`);
  const world =
    values.world === undefined
      ? undefined
      : World.parse(JSON.parse(readFileSync(values.world, "utf8")));
  // The phase the issue stands in, and the word of the handoff record each phase ends with: the
  // ninth word's, for a role that works none, whose phase ends with no record; else the case
  // world's, else the one the role works, with the daemon's words (`workflow.RoleFor` and
  // `handoff-words`, through daemon-pane.go).
  const handoffWords: Record<string, string> =
    phaseArg === undefined ? JSON.parse(runDaemonPane(DAEMON_MODULE, ["handoff-words"])) : {};
  const phase =
    phaseArg ??
    world?.daemon?.phase ??
    runDaemonPane(DAEMON_MODULE, ["phase", role]).split("\n")[0] ??
    "";
  const tree = values.tree ?? issue;
  const issues = values.issues === undefined ? [] : issuesOf(values.issues);
  const claimInWorld = issues.find((each) => each.key === issue);
  if (claimInWorld !== undefined && (claimInWorld.tree !== tree || claimInWorld.phase !== phase)) {
    refuse(
      `--issues ${values.issues} gives the claim's issue ${issue} the tree ${claimInWorld.tree} and the phase ${claimInWorld.phase}, and the command line gives it the tree ${tree} and the phase ${phase}`
    );
  }
  let standin: Standin;
  try {
    standin = startDaemonStandin({
      port: Number(portArg),
      logFile,
      bootToken,
      project,
      issue,
      tree,
      role,
      repository,
      defaultBranch,
      phase,
      handoffWords,
      world,
      clockReal: values.clockReal,
      answers: [
        ...(values.caseAnswers === undefined ? [] : readAnswers(values.caseAnswers)),
        ...(values.answers === undefined ? [] : readAnswers(values.answers)),
      ],
      ghCalls: values.ghCalls,
      contract: values.label === undefined ? undefined : String(labelContract(values.label)),
      scored:
        world === undefined
          ? {
              issues,
              label: values.label,
              dispatch:
                values.dispatchUrl === undefined
                  ? undefined
                  : dispatchOf(values.dispatchUrl, values.dispatchTokenFile ?? ""),
            }
          : undefined,
    });
  } catch (error) {
    refuse(error instanceof Error ? error.message : String(error));
  }
  console.log(`listening on http://127.0.0.1:${standin.port}`);
}
