#!/usr/bin/env bun
/**
 * A stand-in for the Legion daemon that serves exactly the routes a phase worker's `legion` tool
 * hits in a skill scenario: the worker's claim registration (`claims/register`, `claims/ready`,
 * the daemon's claim routes the plugin boots through), the grant the tool mints in-process before
 * each of its own daemon calls (`grants`), the issue's record (`GET /legion/v1/state`, the tool's
 * `read_record`), the phase completion (`handoff/complete`, the tool's `handoff_complete`) and
 * the return of the issue to an earlier phase (`phase/backward`, the tool's
 * `request_backward_move`), the two routes here that redeem a grant. No credential route and no
 * grant file: a pane's `gh` and
 * `git` read the rig's token from the gh files under its `GH_CONFIG_DIR`, which `daemon-pane.go`
 * writes, and no agent runs `legion` from bash.
 *
 * Every request and response body is the Go daemon's (`@legion/contracts/legion-api`): the
 * tool's grant request, its completion, and the state document. The grant rule mirrors the real
 * daemon's: a grant lives for 60 seconds and redeems any number of times while it lives; an
 * unknown or expired grant id answers 403 `{"error":"Invalid or expired grant"}`. Nothing is
 * single-use. A completion is served only while the issue stands in the phase this stand-in's
 * role works: started in any other phase, every `handoff_complete` is refused 409
 * `HANDOFF_NO_RUN`, as the daemon refuses a completion it can attribute to no run of the issue
 * (`packages/daemon/internal/api/handoff.go`, `noRunCompletion`). That is the loop after an
 * approval, which runs in `accepting`, where the daemon starts no phase and the claim holds no
 * task at all. A completion of a phase that ends with a handoff record (daemon-pane.go's handoff
 * word for the role's phase) must name one, `handoffId` and a positive `handoffVersion`, and is
 * refused 400 `HANDOFF_RECORD_REQUIRED` otherwise; a completion of a phase that ends with none
 * and names one is refused 409 `HANDOFF_RECORD_MISMATCH`; both as the real daemon refuses them
 * (`L/api/handoff.go`). Any other completion answers `{}`. A backward move answers `{}` once its
 * grant redeems, as the daemon's does for a phase worker, and names no issue but this one
 * (`ISSUE_NOT_GRANTS`, 400, the daemon's refusal of a worker that moves another issue). The
 * stand-in reads no record from
 * Dispatch: the `legion` tool found the newest record of the phase on Dispatch before posting, and
 * the rig's scorer reads that record back from Dispatch and holds it to the run's session
 * (score.ts testerProof).
 *
 * Every request appends one JSON line to the log file: `{at, path, status, grantId?, sessionId?,
 * mintedGrantId?, handoffId?, handoffVersion?, to?, reason?}`, the run's record of what reached
 * the daemon; a completion's line carries the record it named, and a backward move's the phase it
 * asked for and why (score.ts threadsClosed).
 *
 * Usage: `bun daemon-standin.ts <port> <log file> <boot token file> <project> <issue> <repo>
 * <role> [phase]`
 * The state document holds that one issue, on <repo>, which is the run's own world's repository
 * (rig.sh's $worker_repo, the same name its world.json carries): a record naming another sends
 * the agent's review to a repository the world has not got. The issue stands in [phase], or,
 * where the caller names none, in the first phase its role works (daemon-pane.go's `phase
 * <role>`): the implementer works implementing and retro, and the daemon starts it in either.
 * Prints `listening on http://127.0.0.1:<port>` once the socket is open (port 0: the kernel's
 * pick).
 */
import { randomUUID } from "node:crypto";
import { appendFile } from "node:fs/promises";
import { isLegionRole, roleToken } from "@legion/contracts";
import {
  LegionGrantRequest,
  LegionHandoffCompleteRequest,
  LegionPhaseBackwardRequest,
  LegionStateResponse,
} from "@legion/contracts/legion-api";
import { DAEMON_MODULE, runDaemonPane } from "./daemon-pane";

const GRANT_TTL_MS = 60_000;
const [portArg, logFile, bootTokenFile, project, issue, repo, role, phaseArg = ""] =
  Bun.argv.slice(2);
if (
  portArg === undefined ||
  logFile === undefined ||
  bootTokenFile === undefined ||
  project === undefined ||
  issue === undefined ||
  repo === undefined ||
  role === undefined
) {
  console.error(
    "usage: bun daemon-standin.ts <port> <log file> <boot token file> <project> <issue> <repo> <role> [phase]"
  );
  process.exit(2);
}
if (!isLegionRole(role)) {
  console.error(`${role} is not a Legion role`);
  process.exit(2);
}
/** The owner of the run's repository, which is the one owner the project maps. */
const [REPO_OWNER = ""] = repo.split("/");
if (REPO_OWNER === "" || !repo.includes("/")) {
  console.error(`${repo} is not an <owner>/<name> repository`);
  process.exit(2);
}
/** Encoded exactly as the daemon encodes it (`legion-<project>-<key>-<role>`, lower-cased),
 * since the worker claims this token on the real Envoy and Envoy rejects uppercase. */
const ROLE_TOKEN = roleToken(project, issue, role);
const SESSION_SECRET = "rig-secret";

const startedAt = new Date().toISOString();
/** The phase the role's own worker is started in, the first of the transition table it works,
 * and the word of the handoff record that phase ends with, empty when it ends with none. */
const [ROLE_PHASE = "", HANDOFF_WORD = ""] = runDaemonPane(DAEMON_MODULE, ["phase", role]).split(
  "\n"
);
/** The phase the issue stands in for this run: the caller's, or the role's own when it names
 * none. A caller that names another is a run of a loop the daemon starts no phase for, and a
 * completion there belongs to no run (`handle`'s `handoff/complete`). */
const PHASE = phaseArg === "" ? ROLE_PHASE : phaseArg;
/** `GET /legion/v1/state`: the one issue the worker holds, standing in the phase this stand-in
 * was started for, on the run's own repository. That repository is the world's, given by the
 * caller and never written twice: a record that named one repository while the run's gh
 * stand-in served another had agents query both, the second answering 404, and six of the forty
 * runs of 2026-11-10 submitted their review to the repository the record named. `capabilities` is the
 * deployment's capability report, `strandedClaims` the claims nothing will wake, `heldOutboxRows`
 * the lifecycle rows held behind their issues' running ones, `consumers` the lag of the daemon's
 * durable consumers and `repositories` the project's mapping, each of which the reader requires;
 * the rig has no deployment to report on, its one claim runs, it has no outbox and no NATS, so
 * the first four are the empty lists the daemon's MarshalJSON emits when there is none.
 * `wakesWaiting`, the wakes the pacing holds until their turn, is optional to the reader; the rig
 * carries it empty, as a daemon at this contract serves a caller in no pod holding no wake.
 * `settings` is the daemon's resolved legion.yaml with no credential in it, also required; the rig
 * has no file, so it carries the two keys the reader types at the daemon's defaults. */
const stateIn = (phase: string) =>
  LegionStateResponse.parse({
    daemon: { project, schemaVersion: 1, boots: 1, firstBootAt: startedAt, startedAt },
    admission: { cap: 1, active: [issue], waiting: [], trees: [issue], blocked: [] },
    issues: {
      [issue]: {
        key: issue,
        generation: 1,
        phase,
        status: "in_progress",
        tree: issue,
        repo,
        defaultBranch: "main",
        workers: {},
      },
    },
    pendingStatusWrites: [],
    heldOutboxRows: [],
    strandedClaims: [],
    wakesWaiting: [],
    capabilities: [],
    repositories: {
      owners: [REPO_OWNER],
      mapped: [{ repo, default: true }],
      refused: [],
      withoutWebhookApp: [],
      caches: [],
    },
    // Every key any label's contract requires, not this checkout's alone: a label's agents load
    // the plugin of their own checkout, and `legionSettings` is a loose object in each, so a body
    // carrying both keys is described by both contracts while one carrying either alone is
    // refused by the other. Measured on 2026-11-10: the base label, built from main `ff2a10ef`,
    // whose contract requires `settings.max_fix_attempts`, had every state read of all 30 of its
    // runs refused `a body the daemon API contract does not describe`, so its agents spent turns
    // on the daemon and no reviewer of that label could reach the `read_record` the rule asks
    // for — the negative control of k1 and k2 measured in a world of its own. A key a later
    // contract drops stays here for as long as a label the rig builds still requires it.
    settings: { max_fix_attempts: 3, worker_boot_timeout_seconds: 120 },
    consumers: [],
  });

/** The phase the stand-in serves now, which starts as the phase it was started for and moves
 * when that phase ends. The daemon ends a review round on both halves, the review fact and the
 * completion, and the issue then stands in `accepting`, where the loop after an approval that
 * leaves text findings open runs (`skills/legion-worker/references/review-threads.md`, step 1).
 * That step has the reviewer read its record and send to the implementer only once the record
 * shows `accepting`, so a stand-in whose state never moved would answer `reviewing` for ever and
 * make the step unreachable: a reviewer that obeyed the rule would score as one that never sent.
 * Measured on the batch of 2026-11-08T13:08Z, where all five head runs approved correctly and
 * every one read `toldImplementer=false` for this reason. Only `reviewing` moves: the rig's run
 * ends with any other phase's completion, and no scenario reads the record after it. It moves on
 * any accepted completion of that phase, a round that submitted `REQUEST_CHANGES` included,
 * because this stand-in sees no review: the gh stand-in records those, and the two share nothing.
 * That is wider than the daemon, which moves an issue to `accepting` only on an approval, and it
 * is safe for these scenarios because `reviewer-text-findings` is the one of them that stands in
 * `reviewing` at all, and its `defect` case the score reads by the submission rather than by the
 * record. threads-closed's reviewer completes its round too, but both its halves are started in
 * `accepting` (rig.sh's run_threads_closed), where no completion is accepted and nothing moves.
 * A scenario that read the record after a `REQUEST_CHANGES` would need the review recorded here
 * first. */
let servedPhase = PHASE;

interface LogLine {
  readonly at: string;
  readonly path: string;
  readonly status: number;
  readonly grantId?: string;
  readonly sessionId?: string;
  readonly mintedGrantId?: string;
  readonly handoffId?: string;
  readonly handoffVersion?: number;
  readonly to?: string;
  readonly reason?: string;
}

const expectedBootToken = (await Bun.file(bootTokenFile).text()).trim();
if (expectedBootToken.length === 0) {
  console.error(`boot token file ${bootTokenFile} is empty`);
  process.exit(2);
}

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

/** `{<key>: <value>}` for a value the request carried, and no key at all for one it did not: an
 * absent key is how a line says the body named none, which is what the log's readers take it for
 * (score.ts's DaemonLine). */
function carried<K extends string, V>(key: K, value: V | undefined): Record<K, V> | undefined {
  return value === undefined ? undefined : ({ [key]: value } as Record<K, V>);
}

/** Mirrors the real daemon's rule: present and not yet expired, nothing else. Each route parses
 * its body with its own contract schema first, exactly as the daemon's route table does, and
 * hands the parsed id here. */
function resolveGrant(grantId: string): Response | undefined {
  const grant = grants.get(grantId);
  if (!grant || grant.expiresAt <= Date.now()) return forbidden("Invalid or expired grant");
  return undefined;
}

function handle(path: string, body: unknown): { response: Response; mintedGrantId?: string } {
  switch (path) {
    case "/legion/v1/claims/register": {
      if (stringField(body, "bootToken") !== expectedBootToken) {
        return { response: forbidden("Invalid boot token") };
      }
      return {
        response: json(200, {
          claimToken: ROLE_TOKEN,
          tree: issue,
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
    case "/legion/v1/grants": {
      const parsed = LegionGrantRequest.safeParse(body);
      if (!parsed.success) return { response: json(400, { error: parsed.error.message }) };
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
      const parsed = LegionHandoffCompleteRequest.safeParse(body);
      if (!parsed.success) return { response: json(400, { error: parsed.error.message }) };
      const refused = resolveGrant(parsed.data.grantId);
      if (refused) return { response: refused };
      // The daemon attributes a completion to a run of the issue's own generation and phase, and
      // refuses one it can attribute to none (`noRunCompletion`, with `heldTaskNotCurrent`'s
      // reason for the task the claim holds). A worker started in a phase its role does not work
      // holds no task at all: the loop after an approval runs in `accepting`, where the daemon
      // starts no phase, so neither of its halves may complete one. The message is the daemon's,
      // because the skill has the worker quote it to its architect.
      if (PHASE !== ROLE_PHASE) {
        const held = `this claim holds no task at all, and ${issue} is on generation 1 in ${PHASE}`;
        return {
          response: failure(
            409,
            "HANDOFF_NO_RUN",
            `this completion belongs to no run of ${issue}: ${held}; nothing was recorded. Push nothing further, end this turn without completing and tell the architect; if the daemon queues this claim a generation-1 ${PHASE} task (none may: this run's phase can be another claim's) and its work and handoff are already done, call the same handoff_complete in that task's turn, and never redo the work or write a second handoff`
          ),
        };
      }
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
      // The round the completion ends takes the issue with it: a reviewing phase that completes
      // leaves the issue in `accepting`, which is what the reviewer's next step reads.
      if (PHASE === "reviewing") servedPhase = "accepting";
      return { response: json(200, {}) };
    }
    case "/legion/v1/phase/backward": {
      const parsed = LegionPhaseBackwardRequest.safeParse(body);
      if (!parsed.success) return { response: json(400, { error: parsed.error.message }) };
      const refused = resolveGrant(parsed.data.grantId);
      if (refused) return { response: refused };
      if (parsed.data.issue !== undefined && parsed.data.issue !== issue) {
        return {
          response: failure(
            400,
            "ISSUE_NOT_GRANTS",
            `a phase worker moves only its own issue ${issue}, not ${parsed.data.issue}`
          ),
        };
      }
      return { response: json(200, {}) };
    }
    default:
      return { response: json(404, { error: `no route for ${path}` }) };
  }
}

const server = Bun.serve({
  hostname: "127.0.0.1",
  port: Number(portArg),
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
    const { response, mintedGrantId } =
      request.method === "POST"
        ? handle(path, body)
        : request.method === "GET" && path === "/legion/v1/state"
          ? { response: json(200, stateIn(servedPhase)) }
          : { response: json(404, { error: `no route for ${request.method} ${path}` }) };
    const line: LogLine = {
      at: new Date().toISOString(),
      path,
      status: response.status,
      ...carried("grantId", stringField(body, "grantId")),
      ...carried("sessionId", stringField(body, "sessionId")),
      ...carried("mintedGrantId", mintedGrantId),
      ...carried("handoffId", stringField(body, "handoffId")),
      ...carried("handoffVersion", numberField(body, "handoffVersion")),
      ...carried("to", stringField(body, "to")),
      ...carried("reason", stringField(body, "reason")),
    };
    await appendFile(logFile, `${JSON.stringify(line)}\n`);
    return response;
  },
});

console.log(`listening on http://127.0.0.1:${server.port}`);
