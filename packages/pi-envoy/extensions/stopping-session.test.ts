import { afterEach, describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { ROLE_TOPIC_PREFIX } from "@legion/contracts";
import { consumerDelivery } from "@legion/envoy-client/test/subscribe-answer";
import type { Subprocess } from "bun";

// What a session does with its role claims when its process stops (R16 of
// dispatch://INFRA-3589): on SIGTERM, and on any clean stop, it gives back each role claim it
// holds before it deletes its record, so the session's next process claims the role at once. Each
// instance of the extension runs in an OS process of its own (`test/stopping-session.ts`), which
// this file sends a real SIGTERM: Oh My Pi's own signal handler (`postmortem` of
// `@oh-my-pi/pi-utils`) runs the exit callbacks, waits for them, and ends the process with 143.
// A Legion pane runs Oh My Pi in RPC mode, whose exit disposes no session, so there the SIGTERM
// emits no `session_shutdown`; the interactive mode disposes the session from an exit callback
// of its own, so there both reach the extension. The listener is a fake that models a role claim
// the way the listener weighs one under a single session id (`store.Registry.SetRoleWithPrevious`,
// `roleHeldByAnotherProcess` in `packages/envoy/cmd/listener/role_holder.go`): a soft claim of a
// role held under the claimant's own session id by another process is refused while that claim
// stands, and the release that names the process holding it frees it.

const CHILD = join(import.meta.dir, "..", "test", "stopping-session.ts");
const HOST_STUB = join(import.meta.dir, "..", "scripts", "role-takeover", "host-stub.ts");
const SESSION = "ses_stopping_session";
const ROLE = "stopping-session-controller";
const ROLE_TOPIC = `${ROLE_TOPIC_PREFIX}${ROLE}`;
/** The grace a Legion pane's shim gives Oh My Pi between its SIGTERM and its SIGKILL
 *  (`DefaultGrace`, `packages/daemon/internal/shim`). */
const SHIM_GRACE_MS = 5_000;

/** The claim standing for a role: the session it is held under, and the process that made it. */
interface Claim {
  readonly session: string;
  readonly instance: string | undefined;
}

/** One call the fake listener took, in the order it arrived. */
type Call =
  | { readonly kind: "register"; readonly session: string; readonly instance: string | undefined }
  | {
      readonly kind: "claim";
      readonly role: string;
      readonly session: string;
      readonly instance: string | undefined;
      readonly soft: boolean;
      readonly status: number;
    }
  | {
      readonly kind: "release";
      readonly topics: readonly string[];
      readonly session: string;
      readonly instance: string | undefined;
    }
  | { readonly kind: "delete"; readonly session: string; readonly instance: string | null };

interface FakeListener {
  readonly url: string;
  readonly calls: Call[];
  readonly claims: Map<string, Claim>;
  readonly stop: () => void;
}

/** The process each registration from the `from`th call on named, in the order they came. */
function registeredInstances(listener: FakeListener, from = 0): (string | undefined)[] {
  return listener.calls
    .slice(from)
    .filter((call) => call.kind === "register")
    .map((call) => call.instance);
}

/**
 * A listener over HTTP that answers what a session's start, claim and stop send. With
 * `releaseAnswers` false it takes a release and never answers it, and frees nothing: the stop
 * whose release runs out of time.
 */
function fakeListener(options: { readonly releaseAnswers: boolean }): FakeListener {
  const calls: Call[] = [];
  const claims = new Map<string, Claim>();
  const unanswered = Promise.withResolvers<void>();
  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    async fetch(request) {
      const url = new URL(request.url);
      const body =
        request.method === "POST" ? ((await request.json()) as Record<string, unknown>) : {};
      const session = typeof body.session_id === "string" ? body.session_id : "";
      const instance =
        typeof body.client_instance_id === "string" ? body.client_instance_id : undefined;
      if (url.pathname === "/v1/interests/subscribe") {
        calls.push({ kind: "register", session, instance });
        return Response.json({
          session_id: session,
          machine_id: "test",
          dir: typeof body.dir === "string" ? body.dir : "",
          topics: Array.isArray(body.topics) ? body.topics : [],
          delivery: consumerDelivery(session),
        });
      }
      if (url.pathname === "/v1/roles/set") {
        const role = typeof body.role === "string" ? body.role : "";
        const soft = body.soft === true;
        const standing = claims.get(role);
        // Every claim here stands renewed: the process that made it answered for it inside the
        // live window, as one that stopped a moment ago did.
        const refused =
          soft &&
          standing !== undefined &&
          (standing.session === session
            ? standing.instance !== undefined && standing.instance !== instance
            : standing.session !== body.previous_session_id);
        calls.push({ kind: "claim", role, session, instance, soft, status: refused ? 409 : 200 });
        if (refused) {
          return Response.json(
            {
              error: "the role is held under this session id by another process",
              role,
              holder: standing.session,
            },
            { status: 409 }
          );
        }
        claims.set(role, { session, instance });
        return Response.json({
          session_id: session,
          machine_id: "test",
          dir: "/tmp",
          topics: [`${ROLE_TOPIC_PREFIX}${role}`],
        });
      }
      if (url.pathname === "/v1/interests/unsubscribe") {
        const topics = Array.isArray(body.topics) ? body.topics.map(String) : [];
        calls.push({ kind: "release", topics, session, instance });
        if (!options.releaseAnswers) {
          await unanswered.promise;
          return Response.json({ removed: [] });
        }
        // The release frees the claims naming this session and the process that asks, and no
        // other (`store.RoleReleaseAuthority`).
        for (const topic of topics) {
          if (!topic.startsWith(ROLE_TOPIC_PREFIX)) continue;
          const role = topic.slice(ROLE_TOPIC_PREFIX.length);
          const standing = claims.get(role);
          if (
            standing?.session === session &&
            (standing.instance === undefined || standing.instance === instance)
          ) {
            claims.delete(role);
          }
        }
        return Response.json({ removed: topics });
      }
      const sessionRecord = /^\/v1\/sessions\/([^/]+)$/.exec(url.pathname);
      if (request.method === "DELETE" && sessionRecord !== null) {
        calls.push({
          kind: "delete",
          session: decodeURIComponent(sessionRecord[1] ?? ""),
          instance: url.searchParams.get("client_instance_id"),
        });
        return Response.json({ deleted: true });
      }
      const roleRead = /^\/v1\/roles\/([^/]+)$/.exec(url.pathname);
      if (roleRead !== null) {
        const standing = claims.get(decodeURIComponent(roleRead[1] ?? ""));
        if (standing === undefined) {
          return Response.json({ error: "no live holder" }, { status: 404 });
        }
        return Response.json({
          role: decodeURIComponent(roleRead[1] ?? ""),
          holder: standing.session,
          last_seen: Date.now(),
          ...(standing.instance === undefined ? {} : { client_instance_id: standing.instance }),
        });
      }
      const interestRead = /^\/v1\/interests\/([^/]+)$/.exec(url.pathname);
      if (interestRead !== null) {
        return Response.json({
          session_id: decodeURIComponent(interestRead[1] ?? ""),
          machine_id: "test",
          dir: "/tmp",
          topics: [],
        });
      }
      return Response.json({});
    },
  });
  return {
    url: `http://127.0.0.1:${server.port}`,
    calls,
    claims,
    stop: () => {
      unanswered.resolve();
      void server.stop(true);
    },
  };
}

/** One extension instance in a process of its own, once its session started. */
interface Instance {
  readonly child: Subprocess<"ignore", "pipe", "pipe">;
  /** The transcript the session wrote, which its relaunch carries. */
  readonly entries: readonly unknown[];
  /** What `envoy_role_set` answered, where the instance claimed a role. */
  readonly claim: string | undefined;
}

const children = new Set<Subprocess>();
const listeners = new Set<FakeListener>();
const scratches = new Set<string>();

afterEach(async () => {
  for (const child of children) {
    child.kill("SIGKILL");
    await child.exited;
  }
  children.clear();
  for (const listener of listeners) listener.stop();
  listeners.clear();
  for (const scratch of scratches) rmSync(scratch, { recursive: true, force: true });
  scratches.clear();
});

function startListener(options: { readonly releaseAnswers: boolean }): FakeListener {
  const listener = fakeListener(options);
  listeners.add(listener);
  return listener;
}

/** Reads the instance's first line, which it writes once its session started. */
async function readyLine(
  child: Subprocess<"ignore", "pipe", "pipe">,
  stderr: Promise<string>
): Promise<string> {
  const reader = child.stdout.getReader();
  const decoder = new TextDecoder();
  let text = "";
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      text += decoder.decode(value, { stream: true });
      const end = text.indexOf("\n");
      if (end !== -1) return text.slice(0, end);
    }
  } finally {
    reader.releaseLock();
  }
  throw new Error(
    `the instance ended with status ${await child.exited} before its session started:\n${await stderr}`
  );
}

/** Starts one instance of the extension on `listener`, in a process and a home of its own. */
async function startInstance(
  listener: FakeListener,
  options: {
    readonly mode: "rpc" | "interactive";
    readonly role?: string;
    readonly transcript?: readonly unknown[];
  }
): Promise<Instance> {
  const scratch = mkdtempSync(join(tmpdir(), "envoy-stopping-session-"));
  scratches.add(scratch);
  const configFile = join(scratch, "envoy.json");
  // No Dispatch or Envoy section, and AskGate off: Envoy comes from the variables below alone.
  writeFileSync(configFile, `${JSON.stringify({ askgate: { enabled: false } })}\n`);
  const child = Bun.spawn([process.execPath, "--preload", HOST_STUB, CHILD], {
    cwd: scratch,
    env: {
      PATH: process.env.PATH ?? "",
      HOME: scratch,
      TMPDIR: scratch,
      XDG_STATE_HOME: join(scratch, "state"),
      XDG_DATA_HOME: join(scratch, "data"),
      XDG_CACHE_HOME: join(scratch, "cache"),
      DISPATCH_STATE_DIR: join(scratch, "dispatch-state"),
      ENVOY_CONFIG_FILE: configFile,
      ENVOY_URL: listener.url,
      // Never dialled: the instance's NATS connection is its own stand-in.
      ENVOY_NATS_URL: "nats://nats-under-test:4222",
      ENVOY_TMUX_PLACE_CACHE_MS: "2147483647",
      STOPPING_SESSION_ID: SESSION,
      STOPPING_SESSION_MODE: options.mode,
      STOPPING_SESSION_ROLE: options.role ?? "",
      STOPPING_SESSION_TRANSCRIPT: JSON.stringify(options.transcript ?? []),
    },
    stdin: "ignore",
    stdout: "pipe",
    stderr: "pipe",
  });
  children.add(child);
  // Read from the start, so a process that writes much to it never blocks on a full pipe.
  const stderr = new Response(child.stderr).text();
  const ready = JSON.parse(await readyLine(child, stderr)) as {
    readonly entries: readonly unknown[];
    readonly claim?: string;
  };
  return { child, entries: ready.entries, claim: ready.claim };
}

/** Sends the instance SIGTERM and waits for it to end: its status, and how long it took. */
async function stopWithSigterm(
  instance: Instance
): Promise<{ readonly status: number; readonly ms: number }> {
  const began = performance.now();
  instance.child.kill("SIGTERM");
  const status = await instance.child.exited;
  children.delete(instance.child);
  return { status, ms: performance.now() - began };
}

describe("a stopping session gives back its role claims before it deletes its record", () => {
  test.each([
    "rpc",
    "interactive",
  ] as const)("on SIGTERM in %s mode, and the session's next process then claims the role with no refusal", async (mode) => {
    const listener = startListener({ releaseAnswers: true });
    const first = await startInstance(listener, { mode, role: ROLE });
    expect(first.claim).toBe(`Now holding role: ${ROLE}`);
    const [firstInstance] = registeredInstances(listener);
    expect(firstInstance).toMatch(/^[0-9a-f]{32}$/);
    expect(listener.claims.get(ROLE)).toEqual({ session: SESSION, instance: firstInstance });

    const signalled = listener.calls.length;
    const stopped = await stopWithSigterm(first);
    expect(stopped.status).toBe(143);
    expect(listener.calls.slice(signalled)).toEqual([
      { kind: "release", topics: [ROLE_TOPIC], session: SESSION, instance: firstInstance },
      { kind: "delete", session: SESSION, instance: firstInstance ?? null },
    ]);
    expect(listener.claims.has(ROLE)).toBe(false);

    // The relaunch: a new process of the same session, carrying its transcript, whose start
    // claims the role the transcript records, softly, as every pass does.
    const relaunched = listener.calls.length;
    const second = await startInstance(listener, { mode, transcript: first.entries });
    const [secondInstance] = registeredInstances(listener, relaunched);
    expect(secondInstance).toMatch(/^[0-9a-f]{32}$/);
    expect(secondInstance).not.toBe(firstInstance);
    expect(listener.calls.slice(relaunched).filter((call) => call.kind === "claim")).toEqual([
      {
        kind: "claim",
        role: ROLE,
        session: SESSION,
        instance: secondInstance,
        soft: true,
        status: 200,
      },
    ]);
    expect(listener.claims.get(ROLE)).toEqual({ session: SESSION, instance: secondInstance });
    expect((await stopWithSigterm(second)).status).toBe(143);
  }, 60_000);

  test("a stop whose release never answers still deletes the record, inside the shim's grace, and frees no claim", async () => {
    const listener = startListener({ releaseAnswers: false });
    const first = await startInstance(listener, { mode: "rpc", role: ROLE });
    expect(first.claim).toBe(`Now holding role: ${ROLE}`);
    const [firstInstance] = registeredInstances(listener);

    const signalled = listener.calls.length;
    const stopped = await stopWithSigterm(first);
    expect(stopped.status).toBe(143);
    expect(stopped.ms).toBeLessThan(SHIM_GRACE_MS);
    // One release, never answered and never sent again, and the DELETE after it. The DELETE goes
    // all the same, because it is what lets a message to the stopped session wake it.
    expect(listener.calls.slice(signalled)).toEqual([
      { kind: "release", topics: [ROLE_TOPIC], session: SESSION, instance: firstInstance },
      { kind: "delete", session: SESSION, instance: firstInstance ?? null },
    ]);
    // The stopped process's claim stands until its last renewal is older than the listener's
    // live window, so the session's next process is refused it here.
    expect(listener.claims.get(ROLE)).toEqual({ session: SESSION, instance: firstInstance });
    const relaunched = listener.calls.length;
    const second = await startInstance(listener, { mode: "rpc", transcript: first.entries });
    const [secondInstance] = registeredInstances(listener, relaunched);
    expect(listener.calls.slice(relaunched).filter((call) => call.kind === "claim")).toEqual([
      {
        kind: "claim",
        role: ROLE,
        session: SESSION,
        instance: secondInstance,
        soft: true,
        status: 409,
      },
    ]);
    expect(listener.claims.get(ROLE)).toEqual({ session: SESSION, instance: firstInstance });
    expect((await stopWithSigterm(second)).status).toBe(143);
  }, 60_000);
});
