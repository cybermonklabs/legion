import { beforeEach, describe, expect, test } from "bun:test";
import { createHash } from "node:crypto";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import {
  dispatchToolSpecs,
  IDEMPOTENCY_KEY_HEADER,
  NO_ROUTE_MESSAGE_PREFIX,
} from "@legion/contracts";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { cliRefusals, runDispatchCli } from "../dispatch-cli";
import { commandLine, commandName } from "../dispatch-command";
import { resetAdviceMemory } from "../dispatch-execute";
import { forgetKeyedLayerMemory } from "../dispatch-http";
import { forgetShownPictures } from "../dispatch-picture-tools";
import {
  ARTIFACT_ID,
  answerFor,
  architectureSource,
  ISSUE,
  PLAN_ARTIFACT_ID,
  response,
  UUID,
} from "./command-surface-answers";
import { MAY_HAVE_LANDED, resendOf } from "./resend-line";
import { shellWords } from "./shell-words";

// A `dispatch` write whose answer is lost may have landed. Exit 1 says Dispatch refused it, so a
// caller that reads the status runs the command again, and a new run is a new Idempotency-Key, a
// second write. Every write command exits 75 instead (sysexits' EX_TEMPFAIL, as `envoy-send` does
// for a send it cannot confirm), and names the key and the command that sends the write again
// under it, which Dispatch answers with the first run's answer rather than running it twice.
//
// The census below is one row per `dispatch` command whose example call writes, driven through
// the CLI against a Dispatch that runs its keyed layer: the write lands, its answer is lost in
// transport, and no later attempt gets an answer of Dispatch's. Each row then sends the printed
// command and checks that the write went out under the same key and ran once.

const makeDirectory = temporaryDirectories("dispatch-cli-unsettled");
const SESSION_ID = "ses-1";
const KEY = "0b9c1e7a-55f4-4d2e-9a6b-3c2f1d0e8a71";
const PNG = Buffer.from("89504e470d0a1a0a0000000d49484452", "hex");
/** The one write of `dispatch message`, the command most rows below lose the answer of. */
const MESSAGE_WRITE = { method: "POST", path: `/api/v1/issues/${ISSUE}/messages` };
const MESSAGE = ["message", "--issue", ISSUE, "--body", "Shipped."];
/** How the line opens that says a call sent again a request its first call sent, in place of the
 *  one it built. */
const SWAPPED = "dispatch: warning: this call sent";

let stateDir = "";
beforeEach(() => {
  stateDir = makeDirectory();
  nextProcess();
});

/** A fresh `dispatch` process of the same session: what the library remembers is gone, the
 *  session's state directory stays. */
function nextProcess(): void {
  resetAdviceMemory();
  forgetShownPictures(SESSION_ID);
  forgetKeyedLayerMemory();
}

/** One write the fake Dispatch was sent: a POST or a PATCH, and the Idempotency-Key it carried,
 *  null for none. */
interface Keyed {
  readonly method: string;
  readonly path: string;
  readonly key: string | null;
}

/** A request by its method and path. */
interface Route {
  readonly method: string;
  readonly path: string;
}

interface DispatchOptions {
  /** The write whose answer is lost: its first request runs, then fails in transport. */
  readonly lose?: Route;
  /**
   * What each later attempt under the lost write's key gets: a gateway's rate limit with an hour's
   * `Retry-After`, which no attempt waits out, a transport failure like the first, or a near-copy
   * refusal (`409 POSSIBLE_DUPLICATE`) the keyed layer did not judge, its key not echoed.
   */
  readonly after?: "rate-limited" | "transport" | "possible-duplicate";
  /** The write Dispatch runs and then answers `503` with an hour's `Retry-After`. */
  readonly failAfterRunning?: Route;
  /** The write a gateway answers `429` with an hour's `Retry-After` before it reaches Dispatch. */
  readonly turnAway?: Route;
  /** The write Dispatch refuses, `400`, running nothing. */
  readonly refuse?: Route;
  /** The write Dispatch runs and answers `201`, its body cut off on the way back, the first time. */
  readonly cutBody?: Route;
  /** A Dispatch without the keyed layer: its `/healthz` names none, it echoes no key, and it runs
   *  every write it is sent. */
  readonly noKeyedLayer?: boolean;
  /** A `/healthz` that does not answer, so a call cannot learn whether Dispatch keeps keys. */
  readonly healthDown?: boolean;
  /** The write a Dispatch older than its route answers `404 no route for …`, running nothing,
   *  until `upgrade()`. */
  readonly noRoute?: Route;
  /** The write Dispatch judges a near copy of an issue it holds: `409 POSSIBLE_DUPLICATE`. */
  readonly nearCopy?: Route;
  /** The write something in Dispatch's place answers `201` with `body`, naming no key and running
   *  nothing. */
  readonly forged?: { readonly route: Route; readonly body: string };
}

function matches(route: Route | undefined, method: string, path: string): boolean {
  return route !== undefined && route.method === method && route.path === path;
}

function bodyText(body: unknown): string {
  if (body === undefined || body === null) return "";
  if (typeof body === "string") return body;
  if (body instanceof Uint8Array || body instanceof ArrayBuffer) {
    return new TextDecoder().decode(body);
  }
  throw new Error(`a body the fake does not read: ${Object.prototype.toString.call(body)}`);
}

/** The body as Dispatch's keyed layer digests it: a multipart boundary, which a client draws
 *  afresh for every encoding of one form, masked. */
function digestOf(method: string, url: URL, headers: Headers, body: string): string {
  const boundary = /boundary=([^;]+)/.exec(headers.get("Content-Type") ?? "")?.[1];
  const masked = boundary === undefined ? body : body.replaceAll(boundary, "<boundary>");
  return `${method} ${url.pathname}${url.search}\n${masked}`;
}

function gatewayRateLimit(): Response {
  return new Response("<html><body><h1>429 Too Many Requests</h1></body></html>", {
    status: 429,
    statusText: "Too Many Requests",
    headers: { "Content-Type": "text/html", "Retry-After": "3600" },
  });
}

function echoed(answer: Response, key: string): Response {
  const headers = new Headers(answer.headers);
  headers.set(IDEMPOTENCY_KEY_HEADER, key);
  return new Response(answer.body, { status: answer.status, headers });
}

/**
 * Dispatch behind its keyed layer, answering every route as the command-surface record does: a
 * keyed write runs once and its answer is kept under its key; a repeat is answered with that
 * answer (`Idempotency-Replayed: ?1`), and the same key on another request is `422 KEY_REUSED`.
 * Every answer to a keyed request carries its key back, and `/healthz` says `idempotency_keys`.
 * A write without a key runs every time it is sent. What a command reads before it writes shows
 * what landed: the issue is closed once a close landed, and an upload is among its documents.
 */
function keyedDispatch(options: DispatchOptions = {}) {
  const kept = new Map<string, { digest: string; status: number; text: string; lost: boolean }>();
  /** Each write that ran, in order. */
  const landed: Keyed[] = [];
  /** Each write sent, in order, whatever came of it. */
  const sent: Keyed[] = [];
  let losing = options.lose;
  let cutting = options.cutBody;
  let lostKey: string | undefined;
  let upgraded = false;
  let closedAt: string | undefined;
  const uploads: string[] = [];
  /** What a landed write changes of what the routes the commands read answer. */
  const land = (method: string, path: string, body: string): void => {
    const fields = /^\s*\{/.test(body) ? (JSON.parse(body) as Record<string, unknown>) : {};
    if (method === "PATCH" && path === `/api/v1/issues/${ISSUE}` && fields.status === "done") {
      closedAt = "2026-01-02T03:04:05Z";
    }
    if (path === `/api/v1/issues/${ISSUE}/artifacts` && typeof fields.name === "string") {
      uploads.push(fields.name);
    }
  };
  const answer = async (method: string, path: string, body: unknown): Promise<Response> => {
    // The sync of every source, and the older route that syncs a project's one source.
    if (method === "POST" && path === "/api/v1/projects/CORE/architecture-sources/sync") {
      return response([architectureSource()]);
    }
    if (method === "POST" && path === "/api/v1/projects/CORE/architecture-source/sync") {
      return response(architectureSource());
    }
    const fixed = answerFor(method, path, body);
    if (method !== "GET") return fixed;
    if (path === `/api/v1/issues/${ISSUE}` && closedAt !== undefined) {
      const issue = (await fixed.json()) as Record<string, unknown>;
      return response({ ...issue, status: "done", closed_at: closedAt });
    }
    if (path === `/api/v1/issues/${ISSUE}/artifacts` && uploads.length > 0) {
      const [held] = (await fixed.json()) as Record<string, unknown>[];
      const made = uploads.map((name) => ({
        ...held,
        id: `upload-${name}`,
        name,
        slug: name.replaceAll(".", "-"),
      }));
      return response([held, ...made]);
    }
    return fixed;
  };
  const fetchImpl = (async (input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(String(input));
    const method = init?.method ?? "GET";
    if (url.pathname === "/healthz") {
      if (options.healthDown) throw new TypeError("fetch failed");
      // Dispatch's own health answer (`healthzHandler`): `ok` and `db` on every build, and
      // `idempotency_keys` on one that runs the keyed layer.
      return response(
        options.noKeyedLayer
          ? { ok: true, db: true }
          : { ok: true, db: true, idempotency_keys: true }
      );
    }
    if (method !== "POST" && method !== "PATCH") return answer(method, url.pathname, init?.body);
    const headers = new Headers(init?.headers);
    const key = options.noKeyedLayer ? null : headers.get(IDEMPOTENCY_KEY_HEADER);
    const reply = (given: Response): Response => (key === null ? given : echoed(given, key));
    sent.push({ method, path: url.pathname, key });
    // A route the build does not have reaches no handler and no keyed layer: nothing is kept.
    if (matches(options.noRoute, method, url.pathname) && !upgraded) {
      return response(
        {
          code: "NOT_FOUND",
          error: `${NO_ROUTE_MESSAGE_PREFIX}${method} ${url.pathname}`,
        },
        404
      );
    }
    if (key !== null && key === lostKey) {
      if (options.after === "transport") throw new TypeError("fetch failed");
      if (options.after === "possible-duplicate") return nearCopyRefusal();
      return gatewayRateLimit();
    }
    if (matches(options.turnAway, method, url.pathname)) return gatewayRateLimit();
    const body = bodyText(init?.body);
    const digest = digestOf(method, url, headers, body);
    const held = key === null ? undefined : kept.get(key);
    if (held !== undefined) {
      if (held.digest !== digest) {
        return reply(response({ code: "KEY_REUSED", error: `key ${key} was already used` }, 422));
      }
      if (held.lost) {
        return reply(
          response(
            { code: "KEY_COMMITTED", error: "the write landed and its answer was lost" },
            409
          )
        );
      }
      return reply(
        new Response(held.text, {
          status: held.status,
          headers: { "Content-Type": "application/json", "Idempotency-Replayed": "?1" },
        })
      );
    }
    if (matches(options.refuse, method, url.pathname)) {
      return reply(response({ code: "INVALID", error: "refused" }, 400));
    }
    if (matches(options.nearCopy, method, url.pathname)) return reply(nearCopyRefusal());
    if (matches(options.forged?.route, method, url.pathname)) {
      return new Response(options.forged?.body, {
        status: 201,
        headers: { "Content-Type": "application/json" },
      });
    }
    const ran = await answer(method, url.pathname, init?.body);
    const text = await ran.text();
    if (key !== null) kept.set(key, { digest, status: ran.status, text, lost: false });
    landed.push({ method, path: url.pathname, key });
    land(method, url.pathname, body);
    if (matches(cutting, method, url.pathname)) {
      cutting = undefined;
      const cut = new ReadableStream({
        pull(controller) {
          controller.error(new TypeError("socket closed before the body ended"));
        },
      });
      return reply(
        new Response(cut, { status: ran.status, headers: { "Content-Type": "application/json" } })
      );
    }
    if (matches(options.failAfterRunning, method, url.pathname)) {
      return reply(
        new Response(JSON.stringify({ code: "UNAVAILABLE", error: "publish failed" }), {
          status: 503,
          headers: { "Content-Type": "application/json", "Retry-After": "3600" },
        })
      );
    }
    if (matches(losing, method, url.pathname)) {
      losing = undefined;
      if (key !== null) lostKey = key;
      throw new TypeError("fetch failed");
    }
    return reply(
      new Response(text, { status: ran.status, headers: { "Content-Type": "application/json" } })
    );
  }) as typeof fetch;
  return {
    fetchImpl,
    landed,
    sent,
    /** The network heals, as for the next process: the lost write's key is answered again. */
    heal: () => {
      lostKey = undefined;
    },
    /** Dispatch is upgraded to a build that has the route `noRoute` names. */
    upgrade: () => {
      upgraded = true;
    },
    /** Dispatch made the write under `key` and kept no answer for it, so a repeat is
     *  `409 KEY_COMMITTED`. */
    forgetAnswer: (key: string) => {
      const held = kept.get(key);
      if (held !== undefined) held.lost = true;
    },
  };
}

/** Dispatch's near-copy gate: a create it judged too like an issue it holds, with no candidate. */
function nearCopyRefusal(): Response {
  return response(
    { code: "POSSIBLE_DUPLICATE", error: "A near copy of this issue exists.", candidates: [] },
    409
  );
}

interface RunOptions {
  /** What the call reads from stdin; with none, a read of stdin fails. */
  readonly stdin?: string;
  /** Under Claude Code (its session variable and its 25,000-character output) rather than omp. */
  readonly claude?: boolean;
  /** Settings of the agent's shell to leave out, or to set besides the ones every run has. */
  readonly env?: Readonly<Record<string, string | undefined>>;
}

async function run(
  argv: readonly string[],
  fetchImpl: typeof fetch,
  options: RunOptions = {}
): Promise<{ code: number; stdout: string; stderr: string }> {
  let stdout = "";
  let stderr = "";
  const host = options.claude
    ? { DISPATCH_HOST: "claude", CLAUDE_CODE_SESSION_ID: SESSION_ID }
    : { DISPATCH_HOST: "omp", DISPATCH_SESSION_ID: SESSION_ID };
  const code = await runDispatchCli(
    argv,
    {
      HOME: stateDir,
      DISPATCH_URL: "http://dispatch.test",
      DISPATCH_TOKEN: "test-token",
      DISPATCH_STATE_DIR: stateDir,
      ...host,
      ...options.env,
    },
    {
      stdout: (text) => {
        stdout += text;
      },
      stderr: (text) => {
        stderr += text;
      },
      readText: (path) => {
        if (path !== "-") return readFileSync(path, "utf-8");
        if (options.stdin === undefined) throw new Error("no stdin");
        return options.stdin;
      },
      cwd: stateDir,
      fetchImpl,
    }
  );
  return { code, stdout, stderr };
}

/** Where the CLI keeps the copy of `text`, the `place`-th text a call sent again under `key`
 *  reads: named by its content too, so a later call never replaces it. */
function keptCopy(key: string, place: number, text: string): string {
  const digest = createHash("sha256").update(text).digest("hex").slice(0, 16);
  return join(stateDir, "sessions", SESSION_ID, "resend", `${key}.${place}.${digest}`);
}

/** The line beside the refusal of a call that named `key`, the key of an earlier call's write. */
function earlierWrite(key: string, code: number): string {
  return `dispatch: the earlier call's write may have landed: this call named its idempotency key ${key} and ended without writing it (exit ${code}). Read the write back before you send it again; a run without the key may make it twice.`;
}

/** The example call of `command`, as the words after `dispatch`. */
function exampleArgv(command: string): string[] {
  const spec = dispatchToolSpecs.find((candidate) => commandName(candidate.name) === command);
  if (spec === undefined) throw new Error(`no command ${command}`);
  return shellWords(commandLine(spec.name, spec.example as Record<string, unknown>)).slice(1);
}

interface CensusRow {
  readonly name: string;
  readonly argv: readonly string[];
  /** The write whose answer is lost. */
  readonly lose: Route;
  /** Which of the command's writes that is: the first takes the key, the n-th `<key>.<n>`. */
  readonly nth: number;
  /**
   * The exit of the resend when the command reads what the first call wrote and refuses before it
   * sends the write again (an upload's name the issue now holds, an issue now closed): no second
   * write either way. Absent, the resend sends the write under its key and Dispatch replays it.
   */
  readonly refused?: number;
}

/** Each `dispatch` command whose example call writes, and the write it makes. */
const EXAMPLE_WRITES: readonly (readonly [string, string, string])[] = [
  ["architecture-sync", "POST", "/api/v1/projects/CORE/architecture-sources/acme/infra/sync"],
  ["artifact", "POST", `/api/v1/issues/${ISSUE}/artifacts`],
  ["ask", "POST", `/api/v1/issues/${ISSUE}/asks`],
  ["claim", "POST", `/api/v1/issues/${ISSUE}/claim`],
  ["comment", "POST", `/api/v1/issues/${ISSUE}/comments`],
  ["doc-edit", "POST", `/api/v1/artifacts/${ARTIFACT_ID}/edits`],
  ["edit-ask", "PATCH", `/api/v1/asks/${UUID}`],
  ["handoff-write", "POST", `/api/v1/issues/${ISSUE}/handoffs/implement`],
  ["issue", "POST", "/api/v1/issues"],
  ["issue-update", "PATCH", `/api/v1/issues/${ISSUE}`],
  ["message", "POST", `/api/v1/issues/${ISSUE}/messages`],
  ["plan-approve", "POST", `/api/v1/artifacts/${PLAN_ARTIFACT_ID}/plan-reviews`],
  ["project", "POST", "/api/v1/projects"],
  ["request-approval", "POST", `/api/v1/artifacts/${ARTIFACT_ID}/approval-requests`],
  ["resolve-ask", "POST", `/api/v1/asks/${UUID}/resolve`],
  ["resolve-comment", "POST", `/api/v1/comments/${UUID}/resolve`],
  ["result", "POST", `/api/v1/issues/${ISSUE}/results`],
  ["review", "POST", `/api/v1/artifacts/${ARTIFACT_ID}/versions/2/reviews`],
  ["send-to-agent", "POST", `/api/v1/agents/${UUID}/messages`],
  ["suggest", "POST", `/api/v1/issues/${ISSUE}/comments`],
];

const closeWithReason = [
  "issue-update",
  "--issue",
  ISSUE,
  "--status",
  "done",
  "--reason",
  "Shipped in owner/repo#7.",
];
const commentWithPicture = [
  "comment",
  "--issue",
  ISSUE,
  "--body",
  "Looks good.",
  "--image",
  "shot.png",
];

const CENSUS: readonly CensusRow[] = [
  ...EXAMPLE_WRITES.map(([command, method, path]) => ({
    name: `dispatch ${command}`,
    argv: exampleArgv(command),
    lose: { method, path },
    nth: 1,
    ...(command === "artifact" ? { refused: 2 } : {}),
  })),
  // Two commands make two writes: each is lost in its own row, so the second write's key is
  // shown to be the same on the resend as on the run it was lost in.
  {
    name: "dispatch issue-update --status done --reason, its closing reason",
    argv: closeWithReason,
    lose: { method: "POST", path: `/api/v1/issues/${ISSUE}/messages` },
    nth: 1,
  },
  {
    name: "dispatch issue-update --status done --reason, its close",
    argv: closeWithReason,
    lose: { method: "PATCH", path: `/api/v1/issues/${ISSUE}` },
    nth: 2,
    refused: 1,
  },
  {
    name: "dispatch comment --image, its picture",
    argv: commentWithPicture,
    lose: { method: "POST", path: `/api/v1/issues/${ISSUE}/artifacts` },
    nth: 1,
  },
  {
    name: "dispatch comment --image, its comment",
    argv: commentWithPicture,
    lose: { method: "POST", path: `/api/v1/issues/${ISSUE}/comments` },
    nth: 2,
  },
];

describe("a write whose answer is lost", () => {
  test("the census holds every command whose example call writes", async () => {
    // Each example sent under a key of the caller's: a command made a write of its own when a
    // keyed request carried that key (a POST that writes nothing carries a key of its own).
    const writing: string[] = [];
    for (const spec of dispatchToolSpecs) {
      stateDir = makeDirectory();
      nextProcess();
      const dispatch = keyedDispatch();
      const command = commandName(spec.name);
      const { code, stdout } = await run(
        [...exampleArgv(command), "--idempotency-key", KEY],
        dispatch.fetchImpl
      );
      expect({ command, code }, stdout).toEqual({ command, code: 0 });
      if (dispatch.sent.some((request) => request.key?.startsWith(KEY))) writing.push(command);
    }
    expect(writing.sort()).toEqual(EXAMPLE_WRITES.map(([command]) => command).sort());
  });

  for (const row of CENSUS) {
    test(`${row.name}: exits 75 naming its key and the command that resends it, which writes it once`, async () => {
      writeFileSync(join(stateDir, "shot.png"), PNG);
      const dispatch = keyedDispatch({ lose: row.lose });

      const first = await run(row.argv, dispatch.fetchImpl);
      expect(first.code, first.stdout).toBe(75);
      const { key, cwd, words } = resendOf(first.stdout);
      // The command runs where the call ran, so a relative path it holds (a picture) still reads.
      expect(cwd).toBe(stateDir);
      expect(words).toEqual(["dispatch", ...row.argv, "--idempotency-key", key]);
      const lostKey = row.nth === 1 ? key : `${key}.${row.nth}`;
      expect(first.stdout).toContain(`${MAY_HAVE_LANDED} (idempotency key ${lostKey});`);
      const lost = (request: Keyed) =>
        request.method === row.lose.method && request.path === row.lose.path;
      expect(dispatch.landed.filter(lost)).toEqual([{ ...row.lose, key: lostKey }]);

      dispatch.heal();
      nextProcess();
      const before = dispatch.sent.length;
      const again = await run(words.slice(1), dispatch.fetchImpl);
      const resent = dispatch.sent.slice(before).filter(lost);
      expect(dispatch.landed.filter(lost)).toHaveLength(1);
      if (row.refused === undefined) {
        // The resend sent the write again under the key it had, and Dispatch answered it from the
        // first run: a resend without the key would have run it a second time.
        expect(again.code, again.stdout).toBe(0);
        expect(resent.length).toBeGreaterThan(0);
        expect(resent.map((request) => request.key)).toEqual(resent.map(() => lostKey));
        expect(again.stdout).not.toContain(MAY_HAVE_LANDED);
        // The request it built is the one the first call sent, so nothing was swapped.
        expect(again.stdout).not.toContain(SWAPPED);
      } else {
        // The command read what the first call wrote and refused before it sent anything, and
        // says beside the refusal that the earlier call's write may have landed.
        expect(again.code, again.stdout).toBe(row.refused);
        expect(resent).toEqual([]);
        expect(again.stdout).toContain(earlierWrite(key, row.refused));
      }
    });
  }

  test("a write whose every attempt loses its answer in transport exits 75", async () => {
    const dispatch = keyedDispatch({ lose: MESSAGE_WRITE, after: "transport" });
    const first = await run(MESSAGE, dispatch.fetchImpl);
    expect(first.code, first.stdout).toBe(75);
    const { key, words } = resendOf(first.stdout);
    expect(words).toEqual(["dispatch", ...MESSAGE, "--idempotency-key", key]);
    // Five attempts on the 1, 2, 4 and 8 s schedule, every one under the key.
    expect(dispatch.sent.map((request) => request.key)).toEqual(Array(5).fill(key));
  }, 30_000);

  // The write ran and its status came back; the connection broke before its body did. It landed
  // with its answer lost, and the resend under its key reads that answer back.
  test("a write whose 201 lost its body exits 75, and its resend prints the first answer", async () => {
    const dispatch = keyedDispatch({ cutBody: MESSAGE_WRITE });
    const first = await run(MESSAGE, dispatch.fetchImpl);
    expect(first.code, first.stdout).toBe(75);
    const { key, words } = resendOf(first.stdout);
    expect(dispatch.landed).toEqual([{ ...MESSAGE_WRITE, key }]);
    nextProcess();
    const again = await run(words.slice(1), dispatch.fetchImpl);
    expect(again.code, again.stdout).toBe(0);
    expect(again.stdout).toStartWith("Posted message");
    expect(dispatch.landed).toHaveLength(1);
  });

  test("a write Dispatch answered 5xx after running it exits 75", async () => {
    const dispatch = keyedDispatch({ failAfterRunning: MESSAGE_WRITE });
    const first = await run(MESSAGE, dispatch.fetchImpl);
    expect(first.code, first.stdout).toBe(75);
    expect(first.stdout).toStartWith("UNAVAILABLE: publish failed");
    expect(resendOf(first.stdout).key).toBe(dispatch.landed[0]?.key ?? "");
  });

  test("a resend whose answer is lost again names the same key once", async () => {
    const dispatch = keyedDispatch({ lose: MESSAGE_WRITE });
    const resend = [...MESSAGE, "--idempotency-key", KEY];
    const first = await run(resend, dispatch.fetchImpl);
    expect(first.code, first.stdout).toBe(75);
    expect(resendOf(first.stdout)).toEqual({
      key: KEY,
      cwd: stateDir,
      words: ["dispatch", ...resend],
    });
    expect(dispatch.landed).toEqual([{ ...MESSAGE_WRITE, key: KEY }]);
  });

  // The write landed and Dispatch kept no answer for it, so the resend is answered
  // `409 KEY_COMMITTED`: it is done, and the call succeeds (`DispatchWriteLandedError`).
  test("a resend Dispatch answers KEY_COMMITTED exits 0: the write is done", async () => {
    const dispatch = keyedDispatch({ lose: MESSAGE_WRITE });
    const first = await run(MESSAGE, dispatch.fetchImpl);
    const { key, words } = resendOf(first.stdout);
    dispatch.heal();
    dispatch.forgetAnswer(key);
    nextProcess();
    const again = await run(words.slice(1), dispatch.fetchImpl);
    expect(again.code, again.stdout).toBe(0);
    expect(again.stdout).toContain("(KEY_COMMITTED)");
    expect(dispatch.landed).toHaveLength(1);
  });

  // zsh's EQUALS option reads a bare `=word` as the path of the command `word`, so the resend
  // would send another body than the first run's: a word that opens with `=` is quoted.
  test("the resend command quotes a word that opens with =", async () => {
    const dispatch = keyedDispatch({ lose: MESSAGE_WRITE });
    const message = ["message", "--issue", ISSUE, "--body", "=ls"];
    const first = await run(message, dispatch.fetchImpl);
    expect(first.code, first.stdout).toBe(75);
    const { key, words } = resendOf(first.stdout);
    expect(first.stdout).toContain(
      `&& dispatch message --issue ${ISSUE} --body '=ls' --idempotency-key ${key}\n`
    );
    expect(words).toEqual(["dispatch", ...message, "--idempotency-key", key]);
  });

  // Dispatch keeps a key for the login that sent it, so the resend reaches it only through the
  // settings the first run found Dispatch, its login and its session through, the config file
  // among them. A command that carried them as assignments would carry no AskGate marker, and
  // DISPATCH_TOKEN is a secret, so the line names them rather than spelling them out. The key's
  // window runs from the first call that used it, not from this one.
  test("the resend line names the settings and the config file the call read, and no secret", async () => {
    const dispatch = keyedDispatch({ lose: MESSAGE_WRITE });
    const first = await run(MESSAGE, dispatch.fetchImpl);
    expect(first.code, first.stdout).toBe(75);
    const line = first.stdout.slice(first.stdout.indexOf(MAY_HAVE_LANDED));
    expect(line).toContain(
      "within 24 hours of the first call that used that key, and Dispatch makes each of its writes at most once"
    );
    expect(line).toContain(
      `from a shell where these are set as they were for this call: DISPATCH_HOST, DISPATCH_SESSION_ID, DISPATCH_URL, DISPATCH_TOKEN, HOME; this call read no envoy.json (none is at ${join(stateDir, ".config", "legion", "envoy.json")}), and`
    );
    expect(line).not.toContain("test-token");

    // Under Claude Code its own session variable and config directory choose the session and the
    // file, and the line names those, and the file the call read where there is one.
    stateDir = makeDirectory();
    nextProcess();
    const claudeDir = join(stateDir, "claude");
    mkdirSync(claudeDir, { recursive: true });
    writeFileSync(join(claudeDir, "envoy.json"), "{}\n");
    const tokenFile = join(stateDir, "token");
    writeFileSync(tokenFile, "test-token\n", { mode: 0o600 });
    const claude = await run(MESSAGE, keyedDispatch({ lose: MESSAGE_WRITE }).fetchImpl, {
      claude: true,
      env: {
        CLAUDE_CONFIG_DIR: claudeDir,
        DISPATCH_TOKEN: undefined,
        DISPATCH_TOKEN_FILE: tokenFile,
      },
    });
    expect(claude.code, claude.stdout).toBe(75);
    expect(claude.stdout).toContain(
      `from a shell where these are set as they were for this call: DISPATCH_HOST, CLAUDE_CODE_SESSION_ID, DISPATCH_URL, DISPATCH_TOKEN_FILE, CLAUDE_CONFIG_DIR, HOME; this call read Dispatch's settings from ${join(claudeDir, "envoy.json")}, and`
    );
  });

  // A body the call read from stdin is gone when the printed command runs, and a file it read
  // can change: the CLI keeps a copy of each, and the command reads the copy, so it sends what
  // the first call sent with no stdin at all.
  test("a body read from stdin is kept, so the printed command sends it again with no stdin", async () => {
    const dispatch = keyedDispatch({ lose: MESSAGE_WRITE });
    const fromStdin = ["message", "--issue", ISSUE, "--body-file", "-"];
    const body = "Shipped.\n\nEOF\n$HOME and `pwd` stay as written.\n";
    const first = await run(fromStdin, dispatch.fetchImpl, { stdin: body });
    expect(first.code, first.stdout).toBe(75);
    const { key, words } = resendOf(first.stdout);
    const copy = keptCopy(key, 1, body);
    expect(words).toEqual(["dispatch", ...fromStdin.slice(0, -1), copy, "--idempotency-key", key]);
    expect(readFileSync(copy, "utf-8")).toBe(body);

    dispatch.heal();
    nextProcess();
    const before = dispatch.sent.length;
    const again = await run(words.slice(1), dispatch.fetchImpl);
    expect(again.code, again.stdout).toBe(0);
    expect(dispatch.sent.slice(before)).toEqual([{ ...MESSAGE_WRITE, key }]);
    expect(dispatch.landed).toHaveLength(1);
  });

  // Claude Code shows a command's first 25,000 characters. A body longer than that given inline
  // would cut the printed command off before its key: the CLI moves a long value to a copy the
  // command reads through the flag's file twin, so the whole line fits.
  test("under Claude Code a body too long to print is kept, and the whole line fits", async () => {
    const dispatch = keyedDispatch({ lose: MESSAGE_WRITE });
    const body = "word ".repeat(6_000).trimEnd();
    const message = ["message", "--issue", ISSUE, "--body", body];
    const first = await run(message, dispatch.fetchImpl, { claude: true });
    expect(first.code, first.stdout.slice(0, 2_000)).toBe(75);
    expect(first.stdout.length).toBeLessThan(25_000);
    const { key, words } = resendOf(first.stdout);
    const copy = keptCopy(key, 1, body);
    expect(words).toEqual([
      "dispatch",
      "message",
      "--issue",
      ISSUE,
      "--body-file",
      copy,
      "--idempotency-key",
      key,
    ]);
    expect(readFileSync(copy, "utf-8")).toBe(body);

    dispatch.heal();
    nextProcess();
    const again = await run(words.slice(1), dispatch.fetchImpl, { claude: true });
    expect(again.code, again.stdout).toBe(0);
    expect(dispatch.landed).toHaveLength(1);
  });

  // A Dispatch whose /healthz answered and named no keyed layer runs a write sent again as a
  // second write, so the CLI prints no command that resends it: only that it may have landed.
  test("a write a Dispatch with no keyed layer may have landed exits 75 and offers no resend", async () => {
    const dispatch = keyedDispatch({ lose: MESSAGE_WRITE, noKeyedLayer: true });
    const first = await run(MESSAGE, dispatch.fetchImpl);
    expect(first.code, first.stdout).toBe(75);
    expect(first.stdout).toMatch(
      /\ndispatch: this write may have landed \(idempotency key [0-9a-f-]{36}\); its answer was lost, and this Dispatch says it keeps no idempotency keys, so sending it again may make it twice: read the write back before you send it again\.\n$/
    );
    expect(first.stdout).not.toContain("Run: dispatch");
    expect(dispatch.sent).toHaveLength(1);
  });

  // Something in Dispatch's place answers the message's write 201 with an object of another shape
  // and names no key, where Dispatch's keyed layer names it on every answer: it is no answer of
  // Dispatch's, so the write may have landed with its answer lost, never "Posted message
  // undefined", exit 0.
  for (const body of ['{"unexpected":true}', "[{}]", '{"id":null}']) {
    test(`a write answered 201 ${body} without its key exits 75, not a success`, async () => {
      const dispatch = keyedDispatch({ forged: { route: MESSAGE_WRITE, body } });
      const first = await run(MESSAGE, dispatch.fetchImpl);
      expect(first.code, first.stdout).toBe(75);
      expect(first.stdout).not.toContain("undefined");
      const { key, words } = resendOf(first.stdout);
      expect(words).toEqual(["dispatch", ...MESSAGE, "--idempotency-key", key]);
    });
  }

  // A 2xx whose body is lost comes from a Dispatch that made the write. Whether a resend under its
  // key is safe is what Dispatch's /healthz says: one that names no keyed layer gets no command.
  test("a 201 whose body was cut off, from a Dispatch with no keyed layer, offers no resend", async () => {
    const dispatch = keyedDispatch({ cutBody: MESSAGE_WRITE, noKeyedLayer: true });
    const first = await run(MESSAGE, dispatch.fetchImpl);
    expect(first.code, first.stdout).toBe(75);
    expect(first.stdout).toContain("this Dispatch says it keeps no idempotency keys");
    expect(first.stdout).not.toContain("Run: dispatch");
    expect(dispatch.landed).toHaveLength(1);
  });

  // A call that could not learn whether Dispatch keeps keys (its /healthz did not answer) promises
  // nothing: it says to read the write back first, and gives the command for after.
  test("a write lost where the keyed layer could not be confirmed says to read it back first", async () => {
    const dispatch = keyedDispatch({ lose: MESSAGE_WRITE, healthDown: true });
    const first = await run(MESSAGE, dispatch.fetchImpl);
    expect(first.code, first.stdout).toBe(75);
    const { key, words } = resendOf(first.stdout);
    expect(first.stdout).toContain(
      `; its answer was lost, and this call could not confirm that this Dispatch keeps idempotency keys: read the write back first, and only if it is not there, send the call again under its idempotency key ${key}`
    );
    expect(first.stdout).not.toContain("Dispatch makes each of its writes at most once");
    expect(words).toEqual(["dispatch", ...MESSAGE, "--idempotency-key", key]);
  });

  // A create whose answer was lost, asked again and refused as a near copy the keyed layer did not
  // judge: the command returns that refusal rather than throwing it, and it is still exit 75.
  test("a near-copy refusal after the create's answer was lost exits 75, not 1", async () => {
    const lose = { method: "POST", path: "/api/v1/issues" };
    const dispatch = keyedDispatch({ lose, after: "possible-duplicate" });
    const first = await run(exampleArgv("issue"), dispatch.fetchImpl);
    expect(first.code, first.stdout).toBe(75);
    const { key } = resendOf(first.stdout);
    expect(dispatch.landed).toEqual([{ ...lose, key }]);
  });

  // A Dispatch older than the sync of every source has no route for it, and the command falls back
  // to the older route under the same key: no handler ran under it. Sent again once Dispatch has
  // the newer route, the key is held for the older one, so Dispatch refuses rather than import
  // twice.
  test("architecture-sync's fallback route takes the key its first route had", async () => {
    const plural = { method: "POST", path: "/api/v1/projects/CORE/architecture-sources/sync" };
    const singular = { method: "POST", path: "/api/v1/projects/CORE/architecture-source/sync" };
    const dispatch = keyedDispatch({ noRoute: plural, lose: singular });
    const argv = ["architecture-sync", "--project", "CORE"];
    const first = await run(argv, dispatch.fetchImpl);
    expect(first.code, first.stdout).toBe(75);
    const { key, words } = resendOf(first.stdout);
    expect(dispatch.landed).toEqual([{ ...singular, key }]);
    expect(new Set(dispatch.sent.map((request) => request.key))).toEqual(new Set([key]));

    dispatch.heal();
    dispatch.upgrade();
    nextProcess();
    const again = await run(words.slice(1), dispatch.fetchImpl);
    expect(again.code, again.stdout).toBe(1);
    expect(again.stdout).toStartWith("KEY_REUSED: ");
    expect(again.stdout).toContain(earlierWrite(key, 1));
    expect(dispatch.landed).toHaveLength(1);
  }, 60_000);

  // What a resend sends is what the first call sent: the session keeps each request a call whose
  // write may have landed sent, and a call under that key sends each again as it was. A picture
  // edited in between, or a value a command reads afresh, would otherwise be another request under
  // the key, which Dispatch refuses.
  test("a picture edited after its upload's answer was lost is sent again as it was first sent", async () => {
    const picture = { method: "POST", path: `/api/v1/issues/${ISSUE}/artifacts` };
    writeFileSync(join(stateDir, "shot.png"), PNG);
    const dispatch = keyedDispatch({ lose: picture });
    const first = await run(commentWithPicture, dispatch.fetchImpl);
    expect(first.code, first.stdout).toBe(75);
    const { key, words } = resendOf(first.stdout);
    writeFileSync(join(stateDir, "shot.png"), Buffer.concat([PNG, Buffer.from("edited")]));
    dispatch.heal();
    nextProcess();
    const again = await run(words.slice(1), dispatch.fetchImpl);
    expect(again.code, again.stdout).toBe(0);
    expect(dispatch.landed.filter((request) => request.path === picture.path)).toEqual([
      { ...picture, key },
    ]);
  });

  // The resend sends what the first call sent, whatever the call built now, and says so where
  // the two differ: the agent is never told its edited text was posted.
  test("the call's own command sent again with the key sends what the first call sent, and says so", async () => {
    const note = join(stateDir, "note.md");
    writeFileSync(note, "Shipped.\n");
    const dispatch = keyedDispatch({ lose: MESSAGE_WRITE });
    const own = ["message", "--issue", ISSUE, "--body-file", note];
    const first = await run(own, dispatch.fetchImpl);
    const { key } = resendOf(first.stdout);
    writeFileSync(note, "Shipped, and edited since.\n");
    dispatch.heal();
    nextProcess();
    const again = await run([...own, "--idempotency-key", key], dispatch.fetchImpl);
    expect(again.code, again.stdout).toBe(0);
    expect(again.stdout).toContain(
      `\n${SWAPPED} POST ${MESSAGE_WRITE.path} under idempotency key ${key} as its first call sent it, not as this call built it, which differs (in its text, its files or the directory it ran in): Dispatch holds the first call's request, and what differs in this call was not sent.\n`
    );
    expect(dispatch.landed).toHaveLength(1);
  });

  // However many recorded requests replaced what the call built, one line says so: a line per
  // request could fill the 25,000 characters Claude Code shows and push out the line that names
  // the key.
  test("several requests sent as first sent make one line", async () => {
    const comments = { method: "POST", path: `/api/v1/issues/${ISSUE}/comments` };
    const uploads = { method: "POST", path: `/api/v1/issues/${ISSUE}/artifacts` };
    writeFileSync(join(stateDir, "shot.png"), PNG);
    const dispatch = keyedDispatch({ lose: comments });
    const first = await run(commentWithPicture, dispatch.fetchImpl);
    const { key } = resendOf(first.stdout);
    writeFileSync(join(stateDir, "shot.png"), Buffer.concat([PNG, Buffer.from("edited")]));
    dispatch.heal();
    nextProcess();
    const edited = commentWithPicture.map((word) => (word === "Looks good." ? "Edited." : word));
    const again = await run([...edited, "--idempotency-key", key], dispatch.fetchImpl);
    expect(again.code, again.stdout).toBe(0);
    const warnings = again.stdout.split("\n").filter((line) => line.startsWith(SWAPPED));
    expect(warnings).toEqual([
      `${SWAPPED} 2 requests as its first call sent them, not as this call built them, which differ (in their text, their files or the directory it ran in): POST ${uploads.path} under idempotency key ${key}; POST ${comments.path} under idempotency key ${key}.2. Dispatch holds the first call's requests, and what differs in this call was not sent.`,
    ]);
    expect(dispatch.landed).toHaveLength(2);
  });

  // A command too long to print whole would be cut before its key, and a cut command is a new
  // write. The line then names the key and says to send the call's own command with it.
  test("under Claude Code a command too long to print is not printed, and the key is", async () => {
    const ask = { method: "POST", path: `/api/v1/issues/${ISSUE}/asks` };
    const dispatch = keyedDispatch({ lose: ask });
    const argv = [...exampleArgv("ask"), "--option", `Wait: ${"x".repeat(27_000)}`];
    const first = await run(argv, dispatch.fetchImpl, { claude: true });
    expect(first.code, first.stdout.slice(0, 2_000)).toBe(75);
    expect(first.stdout.length).toBeLessThan(25_000);
    expect(first.stdout).not.toContain("Run: dispatch");
    const key = dispatch.landed[0]?.key ?? "";
    expect(first.stdout).toContain(
      `: in ${stateDir}, send this call's own command again with \`--idempotency-key ${key}\` added. Under the key Dispatch makes no write twice: from this session the call sends each request again as its first call sent it, and Dispatch refuses any other request under the key.`
    );
  });

  // A text read from stdin that could not be kept leaves a command that cannot send it: the line
  // names the key and says to send the call's own command with it, and prints no command.
  test("a write whose stdin could not be kept prints no command that could not send it", async () => {
    const dispatch = keyedDispatch({ lose: MESSAGE_WRITE });
    const session = join(stateDir, "sessions", SESSION_ID);
    mkdirSync(session, { recursive: true });
    writeFileSync(join(session, "resend"), "not a directory");
    const fromStdin = ["message", "--issue", ISSUE, "--body-file", "-"];
    const first = await run(fromStdin, dispatch.fetchImpl, { stdin: "Shipped.\n" });
    expect(first.code, first.stdout).toBe(75);
    expect(first.stdout).not.toContain("Run: dispatch");
    const key = dispatch.landed[0]?.key ?? "";
    expect(first.stdout).toContain(`(idempotency key ${key})`);
    expect(first.stdout).toContain(
      `send this call's own command again with \`--idempotency-key ${key}\` added`
    );
  });

  // A resend Dispatch refuses is refused alone: the write the earlier call sent under the key may
  // still have landed, so the CLI says so beside the refusal, and in the session's ledger, rather
  // than leave exit 1 to read as "nothing was written". Here the resend comes from where the
  // session's record of the first request is not (another session), with another body:
  // `422 KEY_REUSED`.
  test("a resend Dispatch refuses says the earlier call's write may still have landed", async () => {
    const dispatch = keyedDispatch({ lose: MESSAGE_WRITE });
    const first = await run(MESSAGE, dispatch.fetchImpl);
    const { key } = resendOf(first.stdout);
    dispatch.heal();
    nextProcess();
    stateDir = makeDirectory();
    const changed = ["message", "--issue", ISSUE, "--body", "Shipped, edited."];
    const again = await run([...changed, "--idempotency-key", key], dispatch.fetchImpl);
    expect(again.code, again.stdout).toBe(1);
    expect(again.stdout).toStartWith("KEY_REUSED: ");
    expect(again.stdout).toContain(`\n${earlierWrite(key, 1)}\n`);
    expect(again.stdout).not.toContain("failed");
    expect(cliRefusals(again.stdout)).toEqual([]);
    expect(ledgerErrors().at(-1)).toContain(earlierWrite(key, 1));
    expect(dispatch.landed).toHaveLength(1);
  });

  // The same holds for a resend refused before it reaches Dispatch: by its own parse (a flag it
  // does not know) or by the session's settings (no host named).
  test("a resend refused before it reaches Dispatch says the earlier call's write may have landed", async () => {
    const dispatch = keyedDispatch();
    const unknownFlag = await run(
      [...MESSAGE, "--idempotency-key", KEY, "--bogus"],
      dispatch.fetchImpl
    );
    expect(unknownFlag.code, unknownFlag.stdout).toBe(2);
    expect(unknownFlag.stdout).toContain(`\n${earlierWrite(KEY, 2)}\n`);
    expect(cliRefusals(unknownFlag.stdout)).toHaveLength(1);
    expect(ledgerErrors().at(-1)).toContain(earlierWrite(KEY, 2));

    const unknownCommand = await run(["nope", "--idempotency-key", KEY], dispatch.fetchImpl);
    expect(unknownCommand.code, unknownCommand.stdout).toBe(2);
    expect(unknownCommand.stdout).toContain(`\n${earlierWrite(KEY, 2)}\n`);

    const noHost = await run([...MESSAGE, "--idempotency-key", KEY], dispatch.fetchImpl, {
      env: { DISPATCH_HOST: undefined },
    });
    expect(noHost.code, noHost.stdout).toBe(2);
    expect(noHost.stdout).toContain(`\n${earlierWrite(KEY, 2)}\n`);
    expect(dispatch.sent).toEqual([]);
  });

  // A refusal the command returns (a near copy) says so too, in its output and its ledger entry.
  test("a resend refused as a near copy notes the earlier write in its ledger entry", async () => {
    const dispatch = keyedDispatch({ nearCopy: { method: "POST", path: "/api/v1/issues" } });
    const resend = await run(
      [...exampleArgv("issue"), "--idempotency-key", KEY],
      dispatch.fetchImpl
    );
    expect(resend.code, resend.stdout).toBe(1);
    expect(resend.stdout).toContain(earlierWrite(KEY, 1));
    expect(ledgerErrors().at(-1)).toContain(earlierWrite(KEY, 1));
  });
});

/** Each error line of the session's ledger, in order. */
function ledgerErrors(): (string | undefined)[] {
  return readFileSync(join(stateDir, "sessions", SESSION_ID, "results.jsonl"), "utf-8")
    .split("\n")
    .filter((line) => line !== "")
    .map((line): unknown => JSON.parse(line))
    .map((entry) =>
      typeof entry === "object" &&
      entry !== null &&
      "error" in entry &&
      typeof entry.error === "string"
        ? entry.error
        : undefined
    );
}

// The controls: the same CLI, with answers that say nothing was written, exits 1 and names no key.
describe("a write that cannot have landed", () => {
  test("a POST that writes nothing exits 1 when its answer is lost", async () => {
    const lose = { method: "POST", path: "/api/v1/projects/CORE/related" };
    const dispatch = keyedDispatch({ lose });
    const { code, stdout } = await run(exampleArgv("related"), dispatch.fetchImpl);
    expect(code, stdout).toBe(1);
    expect(stdout).not.toContain(MAY_HAVE_LANDED);
  });

  test("a write Dispatch refused exits 1", async () => {
    const dispatch = keyedDispatch({ refuse: MESSAGE_WRITE });
    const { code, stdout } = await run(exampleArgv("message"), dispatch.fetchImpl);
    expect(code, stdout).toBe(1);
    expect(stdout).not.toContain(MAY_HAVE_LANDED);
    expect(dispatch.landed).toEqual([]);
  });

  test("a write a gateway turned away before it reached Dispatch exits 1", async () => {
    const dispatch = keyedDispatch({ turnAway: MESSAGE_WRITE });
    const { code, stdout } = await run(exampleArgv("message"), dispatch.fetchImpl);
    expect(code, stdout).toBe(1);
    expect(stdout).not.toContain(MAY_HAVE_LANDED);
    expect(dispatch.landed).toEqual([]);
  });

  // Nothing listened where the URL points, so no attempt carried the write anywhere: a refusal of
  // the connection, exit 1, never a write that may have landed.
  test("a write whose every connection was refused exits 1, not 75", async () => {
    const refused = (async () => {
      throw Object.assign(new TypeError("ECONNREFUSED: Unable to connect."), {
        code: "ECONNREFUSED",
      });
    }) as unknown as typeof fetch;
    const { code, stdout } = await run(MESSAGE, refused);
    expect(code, stdout).toBe(1);
    expect(stdout).not.toContain(MAY_HAVE_LANDED);
  }, 30_000);

  test("an --idempotency-key that is no key the CLI names is a usage error, exit 2", async () => {
    const dispatch = keyedDispatch();
    for (const key of ["retry-1", KEY.toUpperCase(), `${KEY}.2`]) {
      const { code, stdout } = await run(
        [...exampleArgv("message"), "--idempotency-key", key],
        dispatch.fetchImpl
      );
      expect({ key, code }, stdout).toEqual({ key, code: 2 });
      expect(stdout).toContain(
        `--idempotency-key must be the key a write that exited 75 named, a lower-case UUID; got ${JSON.stringify(key)}`
      );
    }
    expect(dispatch.sent).toEqual([]);
  });
});
