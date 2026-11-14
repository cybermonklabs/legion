#!/usr/bin/env bun
/**
 * A stand-in for the Legion daemon that serves exactly the routes a phase worker's `legion` tool
 * hits in a skill scenario: the worker's claim registration (`claims/register`, `claims/ready`,
 * the daemon's claim routes the plugin boots through), the grant the tool mints in-process before
 * each of its own daemon calls (`grants`), the issue's record (`GET /legion/v1/issues/<key>/record`,
 * the tool's `read_record` and the reminder's own check, with `GET /legion/v1/state` beside it),
 * the health route (`GET /healthz`, which the tool's `read_build` reads for the contract this
 * stand-in states on it) and the phase completion (`handoff/complete`, the tool's
 * `handoff_complete`, the one route here that redeems a grant). No credential route and no grant
 * file: a pane's `gh` and `git` read the rig's token from the gh files under its `GH_CONFIG_DIR`,
 * which `daemon-pane.go` writes, and no agent runs `legion` from bash.
 *
 * Every request and response body is the Go daemon's (`@legion/contracts/legion-api`): the
 * tool's grant request, its completion, and the state document. The grant rule mirrors the real
 * daemon's: a grant lives for 60 seconds and redeems any number of times while it lives; an
 * unknown or expired grant id answers 403 `{"error":"Invalid or expired grant"}`. Nothing is
 * single-use. A completion of a phase that ends with a handoff record (daemon-pane.go's handoff
 * word for the role's phase) must name one, `handoffId` and a positive `handoffVersion`, and is
 * refused 400 `HANDOFF_RECORD_REQUIRED` otherwise; a completion of a phase that ends with none
 * and names one is refused 409 `HANDOFF_RECORD_MISMATCH`; both as the real daemon refuses them
 * (`L/api/handoff.go`). Any other completion answers `{}`. The stand-in reads no record from
 * Dispatch: the `legion` tool found the newest record of the phase on Dispatch before posting, and
 * the rig's scorer reads that record back from Dispatch and holds it to the run's session
 * (score.ts testerProof).
 *
 * Every request appends one JSON line to the log file: `{at, path, status, grantId?, sessionId?,
 * mintedGrantId?, handoffId?, handoffVersion?}`, the run's record of what reached the daemon; a
 * completion's line carries the record it named.
 *
 * Usage: `bun daemon-standin.ts <port> <log file> <boot token file> <project> <issue> <role>`
 * The state document holds that one issue, in the phase its role works. Prints
 * `listening on http://127.0.0.1:<port>` once the socket is open (port 0: the kernel's pick).
 */
import { randomUUID } from "node:crypto";
import { appendFile } from "node:fs/promises";
import { isLegionRole, roleToken } from "@legion/contracts";
import {
  LegionGrantRequest,
  LegionHandoffCompleteRequest,
  LegionHealthResponse,
  LegionStateResponse,
} from "@legion/contracts/legion-api";
import pkg from "../../package.json";
import { DAEMON_MODULE, runDaemonPane } from "./daemon-pane";

const GRANT_TTL_MS = 60_000;
const [portArg, logFile, bootTokenFile, project, issue, role] = Bun.argv.slice(2);
if (
  portArg === undefined ||
  logFile === undefined ||
  bootTokenFile === undefined ||
  project === undefined ||
  issue === undefined ||
  role === undefined
) {
  console.error(
    "usage: bun daemon-standin.ts <port> <log file> <boot token file> <project> <issue> <role>"
  );
  process.exit(2);
}
if (!isLegionRole(role)) {
  console.error(`${role} is not a Legion role`);
  process.exit(2);
}
/** Encoded exactly as the daemon encodes it (`legion-<project>-<key>-<role>`, lower-cased),
 * since the worker claims this token on the real Envoy and Envoy rejects uppercase. */
const ROLE_TOKEN = roleToken(project, issue, role);
const SESSION_SECRET = "rig-secret";

const startedAt = new Date().toISOString();
/** The phase the role works and the word of the handoff record it ends with, empty when none. */
const [PHASE = "", HANDOFF_WORD = ""] = runDaemonPane(DAEMON_MODULE, ["phase", role]).split("\n");
/** `GET /legion/v1/state`: the one issue the worker holds, admitted in the phase its role works
 * (`workflow.RoleFor`, through daemon-pane.go), on the rig's one repository. `capabilities` is the
 * deployment's capability report, `strandedClaims` the claims nothing will wake, `heldOutboxRows`
 * the lifecycle rows held behind their issues' running ones, `consumers` the lag of the daemon's
 * durable consumers and `repositories` the project's mapping, each of which the reader requires;
 * the rig has no deployment to report on, its one claim runs, it has no outbox and no NATS, so
 * the first four are the empty lists the daemon's MarshalJSON emits when there is none.
 * `wakesWaiting`, the wakes the pacing holds until their turn, is optional to the reader; the rig
 * carries it empty, as a daemon at this contract serves a caller in no pod holding no wake.
 * `settings` is the daemon's resolved legion.yaml with no credential in it, also required; the rig
 * has no file, so it carries the two keys the reader types at the daemon's defaults. */
const STATE = LegionStateResponse.parse({
  daemon: { project, schemaVersion: 1, boots: 1, firstBootAt: startedAt, startedAt },
  admission: { cap: 1, active: [issue], waiting: [], trees: [issue], blocked: [] },
  issues: {
    [issue]: {
      key: issue,
      generation: 1,
      phase: PHASE,
      status: "in_progress",
      tree: issue,
      repo: "sjawhar/legion-smoke",
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
    owners: ["sjawhar"],
    mapped: [{ repo: "sjawhar/legion-smoke", default: true }],
    refused: [],
    withoutWebhookApp: [],
    caches: [],
  },
  settings: { review_round_cap: 3, worker_boot_timeout_seconds: 120 },
  consumers: [],
});

/** `GET /healthz`: that this stand-in is serving, and the project its state names, which is the
 * one word the controller's boot check reads. The contract goes in the header beside it
 * (below), where the tool's `read_build` reads it. */
const HEALTH = LegionHealthResponse.parse({ status: "ok", project });

/** The response header the daemon states its own contract in
 * (`packages/daemon/internal/api/server.go`). The number is this plugin build's own
 * `legion.daemonApiVersion`, which is the contract a daemon this build pairs with speaks. */
const DAEMON_CONTRACT_HEADER = "Legion-Daemon-Contract";

interface LogLine {
  readonly at: string;
  readonly path: string;
  readonly status: number;
  readonly grantId?: string;
  readonly sessionId?: string;
  readonly mintedGrantId?: string;
  readonly handoffId?: string;
  readonly handoffVersion?: number;
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
    default:
      return { response: json(404, { error: `no route for ${path}` }) };
  }
}

/** `GET /legion/v1/issues/<key>/record`, and the key it names. */
const RECORD_ROUTE = /^\/legion\/v1\/issues\/([^/]+)\/record$/;

/** The answer to one request: the POST routes `handle` serves, then the three GET routes, then
 * the 404 every other request gets. */
function answer(
  method: string,
  path: string,
  body: unknown
): { response: Response; mintedGrantId?: string } {
  if (method === "POST") return handle(path, body);
  if (method === "GET") {
    if (path === "/healthz") {
      return {
        response: Response.json(HEALTH, {
          headers: { [DAEMON_CONTRACT_HEADER]: String(pkg.legion.daemonApiVersion) },
        }),
      };
    }
    if (path === "/legion/v1/state") return { response: json(200, STATE) };
    const record = RECORD_ROUTE.exec(path);
    if (record !== null) {
      const held = STATE.issues[decodeURIComponent(record[1] ?? "")];
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
  return { response: json(404, { error: `no route for ${method} ${path}` }) };
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
    const { response, mintedGrantId } = answer(request.method, path, body);
    const line: LogLine = {
      at: new Date().toISOString(),
      path,
      status: response.status,
      ...(stringField(body, "grantId") === undefined
        ? {}
        : { grantId: stringField(body, "grantId") }),
      ...(stringField(body, "sessionId") === undefined
        ? {}
        : { sessionId: stringField(body, "sessionId") }),
      ...(mintedGrantId === undefined ? {} : { mintedGrantId }),
      ...(stringField(body, "handoffId") === undefined
        ? {}
        : { handoffId: stringField(body, "handoffId") }),
      ...(numberField(body, "handoffVersion") === undefined
        ? {}
        : { handoffVersion: numberField(body, "handoffVersion") }),
    };
    await appendFile(logFile, `${JSON.stringify(line)}\n`);
    return response;
  },
});

console.log(`listening on http://127.0.0.1:${server.port}`);
