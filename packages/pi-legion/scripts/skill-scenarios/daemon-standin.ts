#!/usr/bin/env bun
/**
 * A stand-in for the Legion daemon that serves exactly the routes a Legion role's `legion` tool
 * hits in a skill scenario: the claim's registration (`claims/register`, `claims/ready`, the
 * daemon's claim routes the plugin boots through), the grant the tool mints in-process before each
 * of its own daemon calls (`grants`, in its session form for a phase worker or an architect and its
 * controller form for the controller), the issue's record (`GET /legion/v1/issues/<key>/record`,
 * the tool's `read_record` and the reminder's own check) and the state (`GET /legion/v1/state`, the
 * controller's `read_state`), the health route (`GET /healthz`, which the tool's `read_build`
 * reads for the contract this stand-in states on it), and the writes each role's operations make,
 * every one of which redeems a grant:
 *
 *   phase worker   `handoff_complete` (`handoff/complete`, a step's completion or a production
 *                  check's look report) and `request_backward_move` (`phase/backward`)
 *   architect      those two, and `register_gate` (`gates/register`), `register_plan`
 *                  (`plans/register`), `sign_off` (`signoff`), `release_children`
 *                  (`waves/release`) and `rerun_child` (`children/rerun`)
 *   controller     `set_status` (`issues/status`), and `read_state`
 *
 * No credential route and no grant file: a pane's `gh` and `git` read the rig's token from the gh
 * files under its `GH_CONFIG_DIR`, which `daemon-pane.go` writes, and no agent runs `legion` from
 * bash. A route the `legion` tool has no operation of for these roles (read_release_records,
 * ready_for_update, stop_task, retry_or_escalate, close_root, the controller's other writes) is
 * answered 404 unless an answers file holds an answer for it.
 *
 * Every request body is parsed with its contract schema (`@legion/contracts/legion-api`), as the
 * daemon's route table parses it, and refused 400 when it does not parse. The grant rule mirrors
 * the real daemon's: a grant lives for 60 seconds and redeems any number of times while it lives;
 * an unknown or expired grant id answers 403 `{"error":"Invalid or expired grant"}`. Nothing is
 * single-use. A request that parses and holds a live grant is answered, in this order, from:
 *
 *   1. the answers files. `--case-answers` (the case's own `daemon-answers.json`) and `--answers`
 *      (the shared one), each `{version: 1, captured: {command, at, commit}, answers: [{name,
 *      method, path, match, status, body}]}` as t4-contracts.md defines it: the first entry, the
 *      case's before the shared file's, whose `method` and `path` equal the request's and whose
 *      every `match` value equals the request's at its dot path (`args.wake_at`, `issues.0`; a
 *      `null` holds where the request has no value there) answers it with its `status` and `body`
 *      (a 204 answers no body). A file or an entry with a member this schema does not name is
 *      refused when the stand-in starts.
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
 * one a successful `gh pr create` or POST of `repos/<o>/<r>/pulls` in `--gh-calls` opened and no
 * `gh pr close` or `gh pr merge` since closed). The daemon's other refusals depend on state this
 * stand-in does not hold (the claim's serving run and entry, Dispatch's records, GitHub): a
 * recorded answer (`--answers`) stands for each. `request_backward_move` of another issue than a
 * phase worker's own is refused 400 ISSUE_NOT_GRANTS, `register_gate` of a child 403
 * GATE_ROOT_ONLY, and `set_status` without a controller grant 403 CONTROLLER_REQUIRED.
 *
 * Every request appends one JSON line to the log file, `{at, method, path, status, body, answer,
 * grantId?, sessionId?, handoffId?, handoffVersion?}`: `body` is the request JSON (null for a GET
 * or a body that is no JSON) and `answer` the response JSON (null where the answer has no body,
 * a 204), each with the value of a `secret` or `bootToken` member replaced by "<redacted>". The
 * line carries the record a completion named.
 *
 * Usage: `bun daemon-standin.ts <port> <log file> <boot token file> <project> <issue> <role>
 * <repository> <default branch> [--world <file>] [--tree <root issue>] [--answers <file>]
 * [--case-answers <file>] [--gh-calls <file>]`
 * `<role>` is a Legion role or `controller`. The state document holds that one issue, on that
 * repository and its default branch, in the phase its role works (rig.sh's tester-proof world), or
 * in the case's world: `--world` names a JSON file holding a case's `world` whole
 * (heldout-case.ts `World`, parsed strictly), of which the stand-in reads `daemon.phase`, the
 * `dispatch.issue.status`, the issue's pull request among `github.pullRequests` (the record's
 * `pullRequest`), `daemon.step` (the place a tester's `return_to` is held below) and
 * `daemon.productionCheck` (the record's open `check` on a done issue: its look, whether that look
 * is the last, when it opened, its bound and its merge commit; the tester works it). `daemon.task`
 * is the text of the worker's task, which the daemon sends and its state does not carry, so the
 * rig writes it into the prompt. `--tree` is the root of the issue's tree when the issue is a
 * child's (default: the issue is a root), `--gh-calls` the run's `calls.jsonl`, which gh-standin.ts
 * writes. The document is the world as the run starts: a backward move sets its phase and
 * `set_status` its status, and nothing else moves it. Prints `listening on
 * http://127.0.0.1:<port>` once the socket is open (port 0: the kernel's pick).
 */
import { randomUUID } from "node:crypto";
import { appendFileSync, existsSync, readFileSync } from "node:fs";
import { parseArgs } from "node:util";
import {
  controllerToken,
  isLegionRole,
  type LegionRole,
  roleForPhase,
  roleToken,
} from "@legion/contracts";
import { daemonContract } from "@legion/contracts/daemon-contract";
import {
  LegionActivityRequest,
  LegionChildRequest,
  LegionControllerGrantRequest,
  LegionGateRegisterRequest,
  LegionGrantRequest,
  LegionHandoffCompleteRequest,
  LegionHealthResponse,
  LegionIssueRecordResponse,
  LegionIssueStatusRequest,
  LegionPhaseBackwardRequest,
  LegionPlanRegisterRequest,
  LegionReadyRequest,
  LegionSignOffRequest,
  LegionStateResponse,
  LegionTurnOutcomeRequest,
  LegionWaveReleaseRequest,
} from "@legion/contracts/legion-api";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { z } from "zod";
import pkg from "../../package.json";
import { DAEMON_MODULE, runDaemonPane } from "./daemon-pane";
import { World } from "./heldout-case";

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
  path: string,
  body: unknown
): AnswerEntry | undefined {
  return answers.find(
    (entry) =>
      entry.method === method &&
      entry.path === path &&
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

/** An answer: its status, and its JSON body where it has one. */
interface Reply {
  readonly status: number;
  readonly body?: unknown;
  readonly headers?: Record<string, string>;
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

/** A route's answer, or the refusal it gave before any answer was due. */
type Prepared = { readonly refusal: Reply } | { readonly answer: () => Reply };
type Route = (body: unknown) => Prepared;

/** `handoff/complete`'s body, held to the contract's shape and types and no further: the
 * contract's refinements (a look's report names no commit, a rating comes with its reason,
 * `returnTo` goes with a fail) are refusals of the daemon's each with a code of its own, which
 * `complete` makes in the daemon's order. */
const HandoffCompleteBody = z.strictObject(LegionHandoffCompleteRequest.shape);
type HandoffComplete = z.output<typeof HandoffCompleteBody>;

/** The response header the daemon states its own contract in
 * (`packages/daemon/internal/api/server.go`). */
const DAEMON_CONTRACT_HEADER = "Legion-Daemon-Contract";

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
  /** The recorded answers, the case's before the shared file's. */
  readonly answers?: readonly AnswerEntry[];
  /** The run's `calls.jsonl`, which says which pull requests the run opened and closed. */
  readonly ghCalls?: string;
}

export interface Standin {
  readonly port: number;
  stop(): void;
}

/** One line of the run's `calls.jsonl` (gh-standin.ts): the call's arguments, what each file or
 * stdin it was handed held, and the status it exited with. */
const CallLine = z.looseObject({
  argv: z.array(z.string()),
  files: z.record(z.string(), z.string()).default({}),
  exit: z.number(),
});

/** Whether `legion/<issue>` has an open pull request: the world's, then each successful call of
 * `calls` that opened one (`gh pr create`, a POST of `repos/<o>/<r>/pulls`) or ended one (`gh pr
 * close`, `gh pr merge`), in order. A `gh pr create` that names no head opens the branch the
 * workspace is on. */
function openPullRequest(
  issue: string,
  world: World | undefined,
  calls: string | undefined
): boolean {
  const branch = `legion/${issue}`;
  const names = (head: string) =>
    head === branch || head.endsWith(`:${branch}`) || head === `refs/heads/${branch}`;
  let open =
    world?.github?.pullRequests.some((pr) => pr.state === "open" && pr.head.ref === branch) ??
    false;
  if (calls === undefined || !existsSync(calls)) return open;
  for (const line of readFileSync(calls, "utf8").split("\n")) {
    if (line === "") continue;
    const { argv, files, exit } = CallLine.parse(JSON.parse(line));
    if (exit !== 0) continue;
    if (argv[0] === "pr" && argv[1] === "create") {
      const head = argv.findIndex((arg) => arg === "--head" || arg === "-H");
      const named = argv.find((arg) => arg.startsWith("--head="));
      if (head >= 0) open = names(argv[head + 1] ?? "") || open;
      else if (named !== undefined) open = names(named.slice("--head=".length)) || open;
      else open = true;
    } else if (argv[0] === "pr" && (argv[1] === "close" || argv[1] === "merge")) open = false;
    else if (argv[0] === "api" && /(^|\s)(-X|--method)\s?POST(\s|$)/.test(argv.join(" "))) {
      const pulls = argv.some((arg) => /^\/?repos\/[^/]+\/[^/]+\/pulls$/.test(arg));
      const fields = argv.filter((arg) => arg.startsWith("head=")).map((arg) => arg.slice(5));
      const input = files[argv[argv.indexOf("--input") + 1] ?? ""];
      if (input !== undefined) fields.push(stringField(JSON.parse(input), "head") ?? "");
      if (pulls && fields.some(names)) open = true;
    }
  }
  return open;
}

/** Starts the stand-in daemon for one run. */
export function startDaemonStandin(options: StandinOptions): Standin {
  const { project, issue, tree, role, repository, defaultBranch, world } = options;
  const answers = options.answers ?? [];
  const startedAt = new Date().toISOString();
  /** Encoded exactly as the daemon encodes it (`legion-<project>-<key>-<role>`, lower-cased),
   * since the worker claims this token on the real Envoy and Envoy rejects uppercase. */
  const roleTokenValue =
    role === "controller" ? controllerToken(project) : roleToken(project, issue, role);
  /** The worker's registration secret: random, as the daemon's is, since the pane holds it and
   * scrubs it from every line it prints (`@legion/contracts/held-secrets`). */
  const sessionSecret = fixtureSecret("reg");
  const grants = new Map<string, Grant>();

  const daemonWorld = world?.daemon ?? null;
  /** What moves while the run goes on: the phase a backward move sets, the status `set_status`
   * writes, whether the production check is still open and the looks reported on it. */
  const held = {
    phase: options.phase,
    status: world?.dispatch.issue.status ?? "in_progress",
    checkOpen: daemonWorld?.productionCheck != null,
    reported: new Set<string>(),
  };

  /** The open production check as `GET .../record` shows it (`api.CheckView`): the look the role
   * works is open, and the next look is due at the check's bound, as the daemon's is where the
   * checking agent has named no time. */
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
      lookOpen: true,
      lastWake: {
        kind: check.last ? "bound" : check.look === 1 ? "merge" : "check wake",
        ...(check.look === 1 ? { at: check.openedAt } : {}),
      },
      nextWakeAt: check.boundAt,
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

  /** `GET /legion/v1/state`: the issue (`issueRecord`), live unless it stands in a phase no
   * worker holds a slot in. `capabilities` is the deployment's capability report, `strandedClaims`
   * the claims nothing will wake, `heldOutboxRows` the lifecycle rows held behind their issues'
   * running ones, `consumers` the lag of the daemon's durable consumers and `repositories` the
   * repositories the live records name, each of which the reader requires; the rig has no
   * deployment to report on, its one claim runs, it has no outbox and no NATS, so the first four
   * are the empty lists the daemon's MarshalJSON emits when there is none. `wakesWaiting`, the
   * wakes the pacing holds until their turn, is optional to the reader; the rig carries it empty,
   * as a daemon at this contract serves a caller in no pod holding no wake. `settings` is the
   * daemon's resolved legion.yaml with no credential in it, also required; the rig has no file,
   * so it carries the two keys the reader types at the daemon's defaults. */
  const state = () => {
    const live = !["done", "held", "awaiting_person"].includes(held.phase);
    return LegionStateResponse.parse({
      daemon: { project, schemaVersion: 1, boots: 1, firstBootAt: startedAt, startedAt },
      admission: {
        cap: 1,
        active: live ? [issue] : [],
        waiting: [],
        trees: held.phase === "done" ? [] : [tree],
        blocked: [],
      },
      issues: { [issue]: issueRecord() },
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
  };

  /** `GET /healthz`: that this stand-in is serving, and the project its state names, which is the
   * one word the controller's boot check reads. The contract goes in the header beside it, where
   * the tool's `read_build` reads it: this plugin build's own (`daemonContract()`), which is the
   * contract a daemon this build pairs with speaks. The stand-in runs from a checkout, whose
   * manifest declares none, so it is the frozen list's greatest number plus the count of entry
   * fixtures, the number the pack writes into the manifest it packs. */
  const health = (): Reply => ({
    status: 200,
    body: LegionHealthResponse.parse({ status: "ok", project }),
    headers: { [DAEMON_CONTRACT_HEADER]: String(daemonContract(pkg, import.meta.dir)) },
  });

  /** A route: its request is parsed with `schema` (refused 400 when it does not parse), held to a
   * live grant when `grantOf` names the request's, and then answered by `handle`, unless an
   * answers file answers it first. */
  function route<S extends z.ZodType>(
    schema: S,
    handle: (request: z.output<S>, grant: Grant | undefined) => Reply,
    grantOf?: (request: z.output<S>) => string
  ): Route {
    return (body) => {
      const parsed = schema.safeParse(body);
      if (!parsed.success) return { refusal: json(400, { error: parsed.error.message }) };
      let grant: Grant | undefined;
      if (grantOf !== undefined) {
        grant = grants.get(grantOf(parsed.data));
        if (!grant || grant.expiresAt <= Date.now()) {
          return { refusal: forbidden("Invalid or expired grant") };
        }
      }
      return { answer: () => handle(parsed.data, grant) };
    };
  }
  const grantId = (request: { grantId: string }) => request.grantId;

  /** Whether the worker of this claim is the tree root's architect: the design gate is its alone. */
  const isRoot = issue === tree;

  /** `handoff/complete` for a production check's look report: the daemon's `checkReport`, against
   * the check the world holds open. */
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
    if (Date.parse(opened) !== Date.parse(check.openedAt)) {
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
    held.reported.add(reported);
    if (verdict !== "pending") held.checkOpen = false;
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
    if (roleForPhase(held.phase) !== role) {
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
    "POST /legion/v1/claims/ready": route(LegionReadyRequest, (request) =>
      request.secret === sessionSecret ? { status: 204 } : forbidden("Invalid session secret")
    ),
    // The pane's turn-boundary reports, which the daemon takes with the claim's secret: activity
    // answers 204 with no body, as `acknowledge` reads it, and a turn's outcome answers the run of
    // refused turns its reports end with (`claim.TurnOutcomeResponse`), none here, failing nothing.
    "POST /legion/v1/claims/activity": route(LegionActivityRequest, (request) =>
      request.secret === sessionSecret ? { status: 204 } : forbidden("Invalid session secret")
    ),
    "POST /legion/v1/claims/turn-outcome": route(LegionTurnOutcomeRequest, (request) =>
      "secret" in request && request.secret !== sessionSecret
        ? forbidden("Invalid session secret")
        : answered({ refusedRun: 0, failed: false })
    ),
    "POST /legion/v1/grants": (body) => {
      const controller = role === "controller";
      const parsed = (controller ? LegionControllerGrantRequest : LegionGrantRequest).safeParse(
        body
      );
      if (!parsed.success) return { refusal: json(400, { error: parsed.error.message }) };
      return {
        answer: () => {
          if (parsed.data.secret !== sessionSecret) return forbidden("Invalid session secret");
          const id = randomUUID();
          const expiresAt = Date.now() + GRANT_TTL_MS;
          grants.set(id, { expiresAt, controller });
          return answered({ grantId: id, expiresAt: new Date(expiresAt).toISOString() });
        },
      };
    },
    "POST /legion/v1/handoff/complete": route(HandoffCompleteBody, complete, grantId),
    "POST /legion/v1/phase/backward": route(
      LegionPhaseBackwardRequest,
      (request) => {
        if (role !== "architect" && request.issue !== undefined && request.issue !== issue) {
          return failure(
            400,
            "ISSUE_NOT_GRANTS",
            `a phase worker moves only its own issue ${issue}, not ${request.issue}`
          );
        }
        if (request.issue === undefined || request.issue === issue) held.phase = request.to;
        return answered();
      },
      grantId
    ),
    "POST /legion/v1/gates/register": route(
      LegionGateRegisterRequest,
      () =>
        isRoot
          ? answered()
          : failure(
              403,
              "GATE_ROOT_ONLY",
              `the design gate belongs to the tree root ${tree}; register it there`
            ),
      grantId
    ),
    "POST /legion/v1/plans/register": route(
      LegionPlanRegisterRequest,
      (request) => answered({ plan: { version: request.version } }),
      grantId
    ),
    "POST /legion/v1/signoff": route(LegionSignOffRequest, () => answered(), grantId),
    "POST /legion/v1/waves/release": route(
      LegionWaveReleaseRequest,
      (request) => answered({ released: [...new Set(request.issues)] }),
      grantId
    ),
    "POST /legion/v1/children/rerun": route(LegionChildRequest, () => answered(), grantId),
    "POST /legion/v1/issues/status": route(
      LegionIssueStatusRequest,
      (request, grant) => {
        if (!grant?.controller) {
          return failure(403, "CONTROLLER_REQUIRED", "issue status requires a controller grant");
        }
        if (request.issue === issue) held.status = request.status;
        return answered();
      },
      grantId
    ),
    "GET /healthz": () => ({ answer: health }),
    "GET /legion/v1/state": () => ({ answer: () => answered(state()) }),
  };

  /** `GET /legion/v1/issues/<key>/record`, and the key it names. */
  const RECORD_ROUTE = /^\/legion\/v1\/issues\/([^/]+)\/record$/;
  const resolve = (method: string, path: string): Route | undefined => {
    const exact = routes[`${method} ${path}`];
    if (exact !== undefined || method !== "GET") return exact;
    const record = RECORD_ROUTE.exec(path);
    if (record === null) return undefined;
    return () => ({
      answer: () =>
        decodeURIComponent(record[1] ?? "") === issue
          ? answered(LegionIssueRecordResponse.parse(issueRecord()))
          : failure(404, "RECORD_NOT_FOUND", `The daemon has no record for ${record[1]}`),
    });
  };

  /** The answer to one request: the route's refusal where the request does not parse or holds no
   * live grant, else a recorded answer, else the route's own. */
  const respond = (method: string, path: string, body: unknown): Reply => {
    const prepared = resolve(method, path)?.(body);
    if (prepared !== undefined && "refusal" in prepared) return prepared.refusal;
    const recorded = recordedAnswer(answers, method, path, body);
    if (recorded !== undefined) {
      return recorded.status === 204
        ? { status: 204 }
        : { status: recorded.status, body: recorded.body ?? {} };
    }
    return prepared === undefined
      ? json(404, { error: `no route for ${method} ${path}` })
      : prepared.answer();
  };

  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: options.port,
    async fetch(request) {
      const path = new URL(request.url).pathname;
      let body: unknown;
      if (request.method === "POST") {
        try {
          body = await request.json();
        } catch {
          body = undefined;
        }
      }
      const reply = respond(request.method, path, body);
      const line = {
        at: new Date().toISOString(),
        method: request.method,
        path,
        status: reply.status,
        body: body === undefined ? null : redact(body),
        answer: reply.body === undefined ? null : redact(reply.body),
        ...(stringField(body, "grantId") === undefined
          ? {}
          : { grantId: stringField(body, "grantId") }),
        ...(stringField(body, "sessionId") === undefined
          ? {}
          : { sessionId: stringField(body, "sessionId") }),
        ...(stringField(body, "handoffId") === undefined
          ? {}
          : { handoffId: stringField(body, "handoffId") }),
        ...(numberField(body, "handoffVersion") === undefined
          ? {}
          : { handoffVersion: numberField(body, "handoffVersion") }),
      };
      appendFileSync(options.logFile, `${JSON.stringify(line)}\n`);
      return reply.body === undefined
        ? new Response(null, { status: reply.status, headers: reply.headers })
        : Response.json(reply.body, { status: reply.status, headers: reply.headers });
    },
  });
  return { port: server.port ?? options.port, stop: () => server.stop(true) };
}

const USAGE =
  "usage: bun daemon-standin.ts <port> <log file> <boot token file> <project> <issue> <role> <repository> <default branch> [--world <file>] [--tree <root issue>] [--answers <file>] [--case-answers <file>] [--gh-calls <file>]";

if (import.meta.main) {
  const { values, positionals } = parseArgs({
    args: Bun.argv.slice(2),
    allowPositionals: true,
    options: {
      world: { type: "string" },
      tree: { type: "string" },
      answers: { type: "string" },
      "case-answers": { type: "string" },
      "gh-calls": { type: "string" },
    },
  });
  const [portArg, logFile, bootTokenFile, project, issue, role, repository, defaultBranch] =
    positionals;
  if (
    positionals.length !== 8 ||
    portArg === undefined ||
    logFile === undefined ||
    bootTokenFile === undefined ||
    project === undefined ||
    issue === undefined ||
    role === undefined ||
    repository === undefined ||
    defaultBranch === undefined
  ) {
    console.error(USAGE);
    process.exit(2);
  }
  if (role !== "controller" && !isLegionRole(role)) {
    console.error(`${role} is not a Legion role`);
    process.exit(2);
  }
  const bootToken = (await Bun.file(bootTokenFile).text()).trim();
  if (bootToken.length === 0) {
    console.error(`boot token file ${bootTokenFile} is empty`);
    process.exit(2);
  }
  const world =
    values.world === undefined
      ? undefined
      : World.parse(JSON.parse(readFileSync(values.world, "utf8")));
  const handoffWords = JSON.parse(runDaemonPane(DAEMON_MODULE, ["handoff-words"])) as Record<
    string,
    string
  >;
  // The phase the role works and the word of the handoff record it ends with, from the daemon
  // (`workflow.RoleFor`, through daemon-pane.go), unless the case's world stands the issue in one.
  const phase =
    world?.daemon?.phase ?? runDaemonPane(DAEMON_MODULE, ["phase", role]).split("\n")[0] ?? "";
  const standin = startDaemonStandin({
    port: Number(portArg),
    logFile,
    bootToken,
    project,
    issue,
    tree: values.tree ?? issue,
    role,
    repository,
    defaultBranch,
    phase,
    handoffWords,
    world,
    answers: [
      ...(values["case-answers"] === undefined ? [] : readAnswers(values["case-answers"])),
      ...(values.answers === undefined ? [] : readAnswers(values.answers)),
    ],
    ghCalls: values["gh-calls"],
  });
  console.log(`listening on http://127.0.0.1:${standin.port}`);
}
