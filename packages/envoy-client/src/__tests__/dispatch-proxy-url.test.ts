// packages/envoy-client/src/__tests__/dispatch-proxy-url.test.ts
//
// What `dispatch proxy` says of the configured Dispatch URL, and where it sends each request.
//
// `dispatch proxy --dry-run` and the proxy's own answers name the Dispatch server as `urlForText`
// names a configured URL: whole where it is a plain http(s) URL, and as a fixed phrase where it
// carries user information or a query, which the configuration takes as written and the proxy
// holds nowhere. They printed the URL whole. The answer to a request Dispatch did not answer names
// the failure by its class (`errorClass`), never the runtime's message: that message repeats the
// host it dialed, and a URL whose password holds an `@` dials part of the password. Each print
// check is the designated assertion of cmd/redact-gate's mutation gate: it prints the output it
// read as `REDACT-CAPTURE <capture> <base64>` and fails with a message that begins
// `REDACT-ASSERT <capture>`.
//
// Each request goes to its target joined onto the configured URL's path, the URL's query kept
// ahead of the target's own (`joinUrl`). It went to a copy of the URL with the path and the query
// set from the target, so a configured query was dropped.
//
// Each run is `runDispatchCli(["proxy", …])` in this process, in a pod's environment under
// `DISPATCH_TOKEN_BROKER=1`, as dispatch-proxy.test.ts runs it. The proxy starts its command only
// once its socket listens, so the command's connection to the row's own server says the proxy
// serves, and the row's close of that connection ends the command: no row polls or sleeps, and
// each wait is bounded.

import { afterEach, describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { connect, createServer, type Socket } from "node:net";
import { join } from "node:path";
import { URL_NOT_PRINTED } from "@legion/contracts/log-text";
import { runDispatchCli } from "../dispatch-cli";
import { brokerStandIns } from "./broker-stand-ins";

const TOKEN = "not-a-secret-token";

/** The session every row's proxy acts as (`DISPATCH_SESSION_ID`). */
const SESSION = "ses-1";

/** The command a row's proxy runs: it connects to the port its first argument names, which says
 *  it started, then waits until that connection closes, and exits 0. */
const CONNECT_AND_WAIT = 'exec 3<>"/dev/tcp/127.0.0.1/$0" || exit 1; read -r -u 3 _; exit 0';

/** What names the Dispatch URL where it does not print. */
const NOT_PRINTED = `the configured Dispatch URL ${URL_NOT_PRINTED}`;

/** One proxy run: what it printed so far, and its exit. */
interface ProxyRun {
  readonly socket: string;
  /** Ends the run's command: {@link startedProxy} sets it, and it does nothing before. */
  end: () => void;
  readonly exited: Promise<number>;
  readonly printed: { stdout: string; stderr: string };
}

/** Every run a row started, ended by the hook below whatever the row's outcome. */
const running = new Set<ProxyRun>();

// Registered before the stand-ins' cleanup, so it runs first: each run's command ends while its
// connection is still open, and the proxy exits before the stand-ins it talks to stop.
afterEach(async () => {
  const runs = [...running];
  running.clear();
  for (const run of runs) run.end();
  await within(
    Promise.allSettled(runs.map((run) => run.exited)),
    4_000,
    "every proxy run this row started exited"
  ).catch(() => undefined);
});

const { podEnvironment, servers, stubBroker, tempDir } = brokerStandIns("dispatch-proxy-url");

type Environment = Record<string, string | undefined>;

/** A pod's environment under `DISPATCH_TOKEN_BROKER=1` with `dispatchUrl` as `DISPATCH_URL`. */
function proxyEnvironment(brokerUrl: string, dispatchUrl: string): Environment {
  return {
    PATH: process.env.PATH,
    ...podEnvironment(brokerUrl, dispatchUrl),
    DISPATCH_HOST: "omp",
    DISPATCH_SESSION_ID: SESSION,
    DISPATCH_TOKEN_BROKER: "1",
    DISPATCH_STATE_DIR: tempDir("state"),
  };
}

/** `promise`, or a failure naming `what` once `ms` have passed. */
async function within<T>(promise: Promise<T>, ms: number, what: string): Promise<T> {
  const late = Promise.withResolvers<never>();
  const timer = setTimeout(() => late.reject(new Error(`not done within ${ms} ms: ${what}`)), ms);
  try {
    return await Promise.race([promise, late.promise]);
  } finally {
    clearTimeout(timer);
  }
}

/** Runs `dispatch <argv>` in this process as the entry would, and keeps it for the cleanup. */
function launch(argv: readonly string[], env: Environment, socket = ""): ProxyRun {
  const printed = { stdout: "", stderr: "" };
  const exited = runDispatchCli(argv, env, {
    stdout: (text) => {
      printed.stdout += text;
    },
    stderr: (text) => {
      printed.stderr += text;
    },
    readText: (path) => readFileSync(path, "utf-8"),
    cwd: tempDir("cwd"),
  });
  // A run that throws fails the row that awaits it; one no row awaits is still handled.
  exited.catch(() => undefined);
  const run: ProxyRun = { socket, end: () => undefined, exited, printed };
  running.add(run);
  return run;
}

/** A proxy whose command runs, so its socket listens; fails at once, naming what it printed,
 *  where the run ends before its command starts. */
async function startedProxy(env: Environment): Promise<ProxyRun> {
  const started = Promise.withResolvers<Socket>();
  const control = createServer((connection) => started.resolve(connection));
  const listening = Promise.withResolvers<void>();
  control.once("error", listening.reject);
  control.listen(0, "127.0.0.1", () => listening.resolve());
  await listening.promise;
  servers.push({ stop: () => control.close() });
  const address = control.address();
  const port = address !== null && typeof address === "object" ? address.port : 0;

  const socket = join(tempDir("socket"), "d.sock");
  const run = launch(
    ["proxy", "--socket", socket, "--", "bash", "-c", CONNECT_AND_WAIT, String(port)],
    env,
    socket
  );
  run.exited.then(
    (code) =>
      started.reject(
        new Error(
          `dispatch proxy exited ${code} before its command started; standard error: ${JSON.stringify(run.printed.stderr)}`
        )
      ),
    (error: unknown) => started.reject(error)
  );
  const connection = await within(started.promise, 10_000, "the proxy started its command");
  run.end = () => connection.end();
  return run;
}

/** Ends the run's command, and answers the proxy's exit status. */
async function finish(run: ProxyRun): Promise<number> {
  run.end();
  return await within(run.exited, 5_000, "dispatch proxy exited once its command ended");
}

/** One answer as the tool reads it: its status and its body's text. */
interface Answer {
  readonly status: number;
  readonly body: string;
}

/** A GET of `target` written by hand over a connection to the proxy's socket, which asks the
 *  proxy to close the connection after it, and the answer it read up to the close. */
function rawGet(socket: string, target: string, host = "dispatch"): Promise<Answer> {
  const { promise, resolve, reject } = Promise.withResolvers<Answer>();
  const connection = connect(socket);
  const chunks: Buffer[] = [];
  connection.on("data", (chunk: Buffer) => chunks.push(chunk));
  connection.once("error", reject);
  connection.once("close", () => {
    const bytes = Buffer.concat(chunks).toString("utf8");
    const end = bytes.indexOf("\r\n\r\n");
    const status = Number(bytes.slice(0, bytes.indexOf("\r\n")).split(" ")[1]);
    resolve({ status, body: end < 0 ? "" : bytes.slice(end + 4) });
  });
  connection.write(`GET ${target} HTTP/1.1\r\nHost: ${host}\r\nConnection: close\r\n\r\n`);
  return within(promise, 10_000, `the proxy's answer to GET ${target}`);
}

/** The `{code, error}` body of one of the proxy's own refusals. */
function refusalOf(answer: Answer): { readonly code: string; readonly error: string } {
  const parsed: unknown = JSON.parse(answer.body);
  const { code, error } = parsed as { code?: unknown; error?: unknown };
  return { code: String(code), error: String(error) };
}

/** A Dispatch that answers an empty list and keeps each request's path and query as sent. */
function recordingDispatch(): { url: string; port: number; asked: string[][] } {
  const asked: string[][] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: (request) => {
      const { pathname, search } = new URL(request.url);
      asked.push([pathname, search]);
      return Response.json([]);
    },
  });
  servers.push(server);
  const { port } = server;
  if (port === undefined) throw new Error("the stand-in Dispatch listens on no port");
  return { url: `http://127.0.0.1:${port}`, port, asked };
}

/** A port nothing listens at: a server's, once that server has stopped. */
function deadPort(): number {
  const gone = Bun.serve({ port: 0, hostname: "127.0.0.1", fetch: () => new Response("") });
  const { port } = gone;
  gone.stop(true);
  if (port === undefined) throw new Error("the stopped server listened on no port");
  return port;
}

/** The designated assertion: capture text, then fail where it holds the token. */
function requireNone(capture: string, text: string): void {
  console.log(`REDACT-CAPTURE ${capture} ${Buffer.from(text, "utf8").toString("base64")}`);
  expect(
    text.includes(TOKEN),
    `REDACT-ASSERT ${capture}: the output prints the secret ${TOKEN}: ${text}`
  ).toBe(false);
}

/** A Dispatch URL on `port` that carries the token: in its user information, or in its query. */
const CREDENTIAL_URLS = [
  ["userinfo", (port: number) => `http://user:${TOKEN}@127.0.0.1:${port}`],
  ["query", (port: number) => `http://127.0.0.1:${port}/dispatch?key=${TOKEN}`],
] as const;

describe("dispatch proxy --dry-run names the configured Dispatch URL without printing a credential", () => {
  for (const [name, urlOn] of CREDENTIAL_URLS) {
    test(name, async () => {
      const broker = stubBroker(3600);
      const dispatch = recordingDispatch();
      const env = proxyEnvironment(broker.url, urlOn(dispatch.port));
      const socket = join(tempDir("socket"), "d.sock");
      const run = launch(["proxy", "--dry-run", "--socket", socket, "--", "true"], env);
      const code = await within(run.exited, 10_000, "dispatch proxy --dry-run exited");
      const { stdout, stderr } = run.printed;
      expect({ code, stderr }).toEqual({ code: 0, stderr });
      requireNone(`dispatch-proxy.dry-run/${name}`, `${stdout}${stderr}`);
      expect(stdout.split("\n")).toContain(`Dispatch server: ${NOT_PRINTED}`);
      expect(broker.trades).toEqual([]);
    });
  }

  test("clean", async () => {
    const broker = stubBroker(3600);
    const dispatch = recordingDispatch();
    const env = proxyEnvironment(broker.url, `${dispatch.url}/dispatch`);
    const socket = join(tempDir("socket"), "d.sock");
    const run = launch(["proxy", "--dry-run", "--socket", socket, "--", "true"], env);
    expect(await within(run.exited, 10_000, "dispatch proxy --dry-run exited")).toBe(0);
    expect(run.printed.stdout.split("\n")).toContain(`Dispatch server: ${dispatch.url}/dispatch`);
  });
});

describe("the proxy's refusal of a target that names a host names the configured Dispatch URL without printing a credential", () => {
  for (const [name, urlOn] of CREDENTIAL_URLS) {
    test(name, async () => {
      const broker = stubBroker(3600);
      const dispatch = recordingDispatch();
      const run = await startedProxy(proxyEnvironment(broker.url, urlOn(dispatch.port)));
      const answer = await rawGet(
        run.socket,
        "http://elsewhere.invalid/api/v1/issues",
        "elsewhere.invalid"
      );
      expect(answer.status).toBe(400);
      requireNone(`dispatch-proxy.target-refused/${name}`, answer.body);
      const { code, error } = refusalOf(answer);
      expect(code).toBe("PROXY_TARGET_REFUSED");
      expect(error).toContain(`to the Dispatch server at ${NOT_PRINTED} alone`);
      expect(dispatch.asked).toEqual([]);
      expect(await finish(run)).toBe(0);
    }, 15_000);
  }

  test("clean", async () => {
    const broker = stubBroker(3600);
    const dispatch = recordingDispatch();
    const run = await startedProxy(proxyEnvironment(broker.url, `${dispatch.url}/dispatch`));
    const answer = await rawGet(run.socket, "//elsewhere.invalid/api/v1/issues");
    expect(answer.status).toBe(400);
    expect(refusalOf(answer).error).toContain(
      `to the Dispatch server at ${dispatch.url}/dispatch alone`
    );
    expect(await finish(run)).toBe(0);
  }, 15_000);
});

describe("the proxy's answer when Dispatch does not answer names the configured Dispatch URL and the failure's class without printing a credential", () => {
  for (const [name, urlOn, failure] of [
    ...CREDENTIAL_URLS.map(([name, urlOn]) => [name, urlOn, /^ECONNREFUSED$/] as const),
    // A password holding an `@`, unescaped: the URL parser reads what follows it as the host, so
    // the runtime's own message names part of the password.
    [
      "password-at",
      () => `http://user:p@ssword-${TOKEN}.invalid`,
      /^getaddrinfo (ENOTFOUND|EAI_AGAIN)$/,
    ],
  ] as const) {
    test(name, async () => {
      const broker = stubBroker(3600);
      const run = await startedProxy(proxyEnvironment(broker.url, urlOn(deadPort())));
      const answer = await rawGet(run.socket, "/api/v1/issues");
      expect(answer.status).toBe(502);
      requireNone(`dispatch-proxy.unreachable/${name}`, answer.body);
      const { code, error } = refusalOf(answer);
      expect(code).toBe("PROXY_DISPATCH_UNREACHABLE");
      const head = `Dispatch at ${NOT_PRINTED} did not answer: `;
      expect(error.slice(0, head.length)).toBe(head);
      expect(error.slice(head.length)).toMatch(failure);
      expect(await finish(run)).toBe(0);
    }, 15_000);
  }

  // The twin: the runtime's own message for the password-at URL, which the answer printed before,
  // holds the token.
  test("password-at's runtime message holds the token", async () => {
    const thrown = await fetch(`http://user:p@ssword-${TOKEN}.invalid/api/v1/issues`).then(
      () => undefined,
      (error: unknown) => error
    );
    expect(thrown).toBeInstanceOf(Error);
    expect((thrown as Error).message).toContain(TOKEN);
  }, 15_000);

  test("clean", async () => {
    const broker = stubBroker(3600);
    const url = `http://127.0.0.1:${deadPort()}`;
    const run = await startedProxy(proxyEnvironment(broker.url, url));
    const answer = await rawGet(run.socket, "/api/v1/issues");
    expect(answer.status).toBe(502);
    expect(refusalOf(answer).error).toBe(`Dispatch at ${url} did not answer: ECONNREFUSED`);
    expect(await finish(run)).toBe(0);
  }, 15_000);
});

describe("the proxy sends each request to its target joined onto the configured Dispatch URL's path", () => {
  for (const [name, path, target, asked] of [
    ["query", "/dispatch?key=v", "/api/v1/issues?scope=open", "?key=v&scope=open"],
    ["query-no-target-query", "/dispatch?key=v", "/api/v1/issues", "?key=v"],
    ["no-query", "/dispatch", "/api/v1/issues?scope=open", "?scope=open"],
  ] as const) {
    test(name, async () => {
      const broker = stubBroker(3600);
      const dispatch = recordingDispatch();
      const run = await startedProxy(proxyEnvironment(broker.url, `${dispatch.url}${path}`));
      const answer = await rawGet(run.socket, target);
      expect(answer.status).toBe(200);
      expect(dispatch.asked).toEqual([["/dispatch/api/v1/issues", asked]]);
      expect(await finish(run)).toBe(0);
    }, 15_000);
  }

  // The configured path is cleaned as the target's join cleans it, so the request is sent below
  // it, and the URL's own check (its path under Dispatch's own path) passes it.
  test("path-repeated-slash", async () => {
    const broker = stubBroker(3600);
    const dispatch = recordingDispatch();
    const run = await startedProxy(proxyEnvironment(broker.url, `${dispatch.url}/dispatch//v2`));
    const answer = await rawGet(run.socket, "/api/v1/issues");
    expect(answer.status).toBe(200);
    expect(dispatch.asked).toEqual([["/dispatch/v2/api/v1/issues", ""]]);
    expect(await finish(run)).toBe(0);
  }, 15_000);

  // A `%` that starts no escape is no path `joinUrl` takes: the proxy refuses it and sends
  // nothing, where it sent the path as the target wrote it.
  test("percent-not-escape", async () => {
    const broker = stubBroker(3600);
    const dispatch = recordingDispatch();
    const run = await startedProxy(proxyEnvironment(broker.url, dispatch.url));
    const answer = await rawGet(run.socket, "/api/v1/issues/100%");
    expect(answer.status).toBe(405);
    const { code, error } = refusalOf(answer);
    expect(code).toBe("PROXY_METHOD_REFUSED");
    expect(error).toContain('The target "/api/v1/issues/100%" is no path the proxy can join');
    expect(dispatch.asked).toEqual([]);
    expect(await finish(run)).toBe(0);
  }, 15_000);
});
