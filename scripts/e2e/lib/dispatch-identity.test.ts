import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { mkdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

// lib/dispatch-identity.sh's real-process test. A proof that runs Dispatch starts, through
// dispatch_identity_start, the local stand-in for a cluster's token issuer and Dispatch's own Envoy
// listener, on the proof's NATS, as a deployment gives Dispatch a listener of its own: the
// listener's OIDC pair takes Dispatch's projected token and closes /v1 to every other caller, so
// the sessions' listener, which takes no credential on loopback, stays open beside it. The suite
// runs the lib in a bash with lib/rig.sh sourced, as a proof sources both, against the real
// envoy-listener and dev-oidc-issuer of this branch and a real nats-server (internal/testnats's
// natsd), and judges what Dispatch's listener answers with fetch, never with the lib's own
// helpers. It builds them with go; a host without go skips it, naming what it lacks.

const root = join(import.meta.dir, "..", "..", "..");
const envoy = join(root, "packages", "envoy");
const dir = temporaryDirectory("dispatch-identity");
const bin = join(dir, "bin");
const work = join(dir, "work");
const evidence = join(dir, "evidence");
const skip = skipWithout("go", "curl");

let nats: Bun.Subprocess<"ignore", "pipe", "pipe"> | undefined;
let harness: Bun.Subprocess<"pipe", "pipe", "pipe"> | undefined;
/** The lines dispatch_identity_start left in dispatch_identity_dispatch_env, one per entry. */
let dispatchEnv: string[] = [];

// The shell a proof is, for this lib: rig.sh's globals, a note and a fail, the rig, then the lib.
// It starts Dispatch's identity and listener, prints dispatch_identity_dispatch_env one entry per
// line and `ready`, and holds them until its stdin closes; its EXIT trap is the cleanup a proof
// writes, dispatch_identity_stop.
const shell = `set -Eeuo pipefail
note() { printf 'note: %s\\n' "$*" >&2; }
fail() { printf 'FAIL: %s\\n' "$*" >&2; exit 1; }
root=$1 work=$2 evidence=$3 timeout_hook=
. "$root/scripts/e2e/lib/rig.sh"
. "$root/scripts/e2e/lib/dispatch-identity.sh"
trap dispatch_identity_stop EXIT
dispatch_identity_start "$4" "$5" dispatch-identity-test
printf '%s\\n' "\${dispatch_identity_dispatch_env[@]}" ready
read -r _ || true
`;

/** What a process printed so far, and a wait for a pattern in it. */
type Printed = {
  text: () => string;
  /** Resolves with pattern's first match once the text holds one, and rejects once every stream
   *  ended without one. The hook's own timeout bounds the wait. */
  printed: (pattern: RegExp) => Promise<RegExpMatchArray>;
};

/** Collects streams into one text, settling each wait on every chunk and at their end. */
function watch(what: string, ...streams: ReadableStream<Uint8Array>[]): Printed {
  let text = "";
  let ended = false;
  const waiting: {
    pattern: RegExp;
    resolve: (found: RegExpMatchArray) => void;
    reject: (error: Error) => void;
  }[] = [];
  const settle = () => {
    for (const waiter of waiting.splice(0)) {
      const found = text.match(waiter.pattern);
      if (found) waiter.resolve(found);
      else if (ended)
        waiter.reject(new Error(`${what} ended before it printed ${waiter.pattern}:\n${text}`));
      else waiting.push(waiter);
    }
  };
  void Promise.all(
    streams.map(async (stream) => {
      const decoder = new TextDecoder();
      for await (const chunk of stream) {
        text += decoder.decode(chunk, { stream: true });
        settle();
      }
    })
  ).then(() => {
    ended = true;
    settle();
  });
  const printed = (pattern: RegExp) => {
    const outcome = Promise.withResolvers<RegExpMatchArray>();
    waiting.push({ pattern, resolve: outcome.resolve, reject: outcome.reject });
    settle();
    return outcome.promise;
  };
  return { text: () => text, printed };
}

function build(target: string, out: string): void {
  const built = Bun.spawnSync(["go", "build", "-o", join(bin, out), target], {
    cwd: envoy,
    stdout: "pipe",
    stderr: "pipe",
  });
  if (built.exitCode !== 0) throw new Error(`go build ${target}: ${built.stdout}${built.stderr}`);
}

/** The value of NAME in dispatchEnv, or undefined when no entry names it. */
function entry(name: string): string | undefined {
  const line = dispatchEnv.find((line) => line.startsWith(`${name}=`));
  return line === undefined ? undefined : line.slice(name.length + 1);
}

/** The status Dispatch's listener answers GET /v1/sessions with, under authorization if given. */
async function sessionsStatus(authorization?: string): Promise<number> {
  const url = entry("ENVOY_URL");
  if (url === undefined)
    throw new Error(`dispatch_identity_dispatch_env names no ENVOY_URL: ${dispatchEnv}`);
  const headers: Record<string, string> =
    authorization === undefined ? {} : { Authorization: authorization };
  const answer = await fetch(`${url}/v1/sessions`, { headers, signal: AbortSignal.timeout(5000) });
  await answer.arrayBuffer();
  return answer.status;
}

beforeAll(async () => {
  if (skip) return;
  mkdirSync(bin, { recursive: true });
  mkdirSync(work, { recursive: true });
  mkdirSync(join(evidence, "logs"), { recursive: true });
  build("./cmd/listener", "envoy-listener");
  build("./internal/testnats/cmd/natsd", "natsd");
  nats = Bun.spawn([join(bin, "natsd"), "-p", "-1", "-store_dir", join(dir, "nats-store")], {
    stdin: "ignore",
    stdout: "pipe",
    stderr: "pipe",
  });
  const port = await watch("natsd", nats.stdout, nats.stderr).printed(
    /Listening for client connections on [^\s]*:(\d+)/
  );
  const natsUrl = `nats://127.0.0.1:${port[1]}`;

  harness = Bun.spawn(
    ["bash", "-c", shell, "harness", root, work, evidence, join(bin, "envoy-listener"), natsUrl],
    { stdin: "pipe", stdout: "pipe", stderr: "pipe" }
  );
  const stdout = watch("dispatch_identity_start's stdout", harness.stdout);
  const stderr = watch("dispatch_identity_start's stderr", harness.stderr);
  await stdout.printed(/^ready$/m).catch((error: Error) => {
    throw new Error(`${error.message}\nstderr:\n${stderr.text()}`);
  });
  const printed = stdout.text();
  dispatchEnv = printed.slice(0, printed.indexOf("ready\n")).split("\n").filter(Boolean);
}, 900_000);

afterAll(async () => {
  if (harness && harness.exitCode === null) {
    harness.stdin.end();
    await harness.exited;
  }
  nats?.kill("SIGTERM");
  await nats?.exited;
});

describe.skipIf(skip)("dispatch_identity_start", () => {
  test("Dispatch is told its own listener and its token file, and no shared token", () => {
    expect(dispatchEnv.map((line) => line.slice(0, line.indexOf("=")))).toEqual([
      "ENVOY_URL",
      "ENVOY_ACCOUNT_TOKEN_FILE",
    ]);
    expect(entry("ENVOY_URL")).toMatch(/^http:\/\/127\.0\.0\.1:\d+$/);
    expect(entry("ENVOY_ACCOUNT_TOKEN_FILE")).toBe(join(work, "dispatch-listener-token"));
  });

  test("Dispatch's listener admits the projected token in Dispatch's token file", async () => {
    const token = readFileSync(join(work, "dispatch-listener-token"), "utf8").trim();
    expect(token).toMatch(/^eyJ/);
    expect(await sessionsStatus(`Bearer ${token}`)).toBe(200);
  });

  test("Dispatch's listener refuses a request that presents no credential", async () => {
    expect(await sessionsStatus()).toBe(401);
  });

  test("Dispatch's listener refuses a bearer of the shared token's shape", async () => {
    expect(await sessionsStatus(`Bearer ${"ab".repeat(24)}`)).toBe(401);
  });

  test("Dispatch's listener names its one credential and Dispatch as the role marks' sender", () => {
    const log = readFileSync(join(evidence, "logs", "dispatch_identity_listener.log"), "utf8");
    expect(log).toMatch(
      /listener API auth: oidc http:\/\/127\.0\.0\.1:\d+ for system:serviceaccount:e2e:dispatch"/
    );
    expect(log).toContain(
      'listener role marks: accepted from system:serviceaccount:e2e:dispatch alone"'
    );
  });

  test("dispatch_identity_stop stops Dispatch's listener", async () => {
    const url = entry("ENVOY_URL");
    harness?.stdin.end();
    expect(await harness?.exited).toBe(0);
    await expect(fetch(`${url}/healthz`, { signal: AbortSignal.timeout(5000) })).rejects.toThrow();
  }, 60_000);
});
