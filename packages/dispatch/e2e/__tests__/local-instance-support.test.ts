import { afterEach, describe, expect, test } from "bun:test";
import { join } from "node:path";
import {
  type FakeDispatchServer,
  startFakeDispatch,
} from "@legion/envoy-client/test/fake-dispatch";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import type { ServerWebSocket } from "bun";

import { databaseUrlRefusal, Interrupted, Supervisor, shellQuote } from "../local-instance-support";

const makeDirectory = temporaryDirectories("local-instance-support");

/** True while any process of group `pgid` is alive: signal 0 to the group reaches every member. */
function groupAlive(pgid: number): boolean {
  try {
    process.kill(-pgid, 0);
    return true;
  } catch {
    return false;
  }
}

/** Polls `ready` until it holds or `ms` pass. Used only for what another process does — its write
 *  to a file, or the kernel reaping a killed process, which still answers `kill(pid, 0)` as a
 *  zombie until its parent or init collects it — with no event in this process to await. */
async function eventually(ready: () => boolean | Promise<boolean>, ms = 3_000): Promise<boolean> {
  const deadline = Date.now() + ms;
  while (Date.now() < deadline) {
    if (await ready()) return true;
    await Bun.sleep(25);
  }
  return ready();
}

/** The pid a real child process writes to `path`. */
async function pidFrom(path: string): Promise<number> {
  const read = () =>
    Bun.file(path)
      .text()
      .catch(() => "");
  if (!(await eventually(async () => /^\d+\s*$/.test(await read())))) {
    throw new Error(`no pid in ${path}`);
  }
  return Number(await read());
}

function processAlive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

/** The pid of a process the supervisor started. `node:child_process` types it as optional, since
 *  a process that never started has none; one the supervisor handed back is running, so a missing
 *  pid is this test's own failure. */
function pidOf(running: { readonly child: { readonly pid?: number } }): number {
  const { pid } = running.child;
  if (pid === undefined) throw new Error("the supervisor started a process with no pid");
  return pid;
}

/** One macrotask turn: the runtime reports an unhandled rejection only after the microtask queue
 *  drains, so a test that waits for that report yields one turn rather than a guessed duration. */
const nextTurn = () => new Promise<void>((resolve) => setImmediate(resolve));

/** A per-test limit for two rows below, `"returns as soon as the leaders exit, not after the
 *  force-kill delay"` and `"ends a foreground step that a stop interrupts"`, and for no other row
 *  of this file: a hang guard rather than a bound
 *  sized to what they take. Measured in this pod — 2 CPUs under gVisor, bun 1.4.4, under 6 busy
 *  loops (3 × `nproc`), 12 runs with `--reporter=junit` — their worst readings were 0.147 s and
 *  0.036 s; the slow pod shape is about 2.15 times slower (inferred from dispatch://INFRA-1505's
 *  Verification preamble, measured by nobody), so about 0.32 s there. It matches
 *  `PROCESS_TEST_MS` in `delivery-live-probe.test.ts`, the sibling file whose rows also start
 *  processes. What takes these two rows off whatever budget the run carries is that the limit is
 *  written here at all, not its size: an explicit per-test limit wins over the lane's
 *  `--timeout`, above it as well as below (`costly-row-ms.ts` reads 90 s against the lane's
 *  60 s). What the run's budget still decides is the cost of a hang, not the verdict: a hung row
 *  ends after its own 30 s plus the `afterEach` hook's share of the same budget, which is 5 s on
 *  a direct run and the lane's 60 s in the lane, where the tester read 90 001.05 ms. */
const STOP_ROW_MS = 30_000;

/** A force-kill delay far above a stop row's limit, so the limit catches a `stop` that waits. */
const BEYOND_THE_ROW_MS = 20 * STOP_ROW_MS;

const supervisors: Supervisor[] = [];
function supervisor(): Supervisor {
  const made = new Supervisor();
  supervisors.push(made);
  return made;
}
afterEach(async () => {
  await Promise.all(supervisors.splice(0).map((made) => made.stop(100)));
});

describe("Supervisor.stop", () => {
  test("ends every process group it started, a grandchild included", async () => {
    const pidFile = join(makeDirectory(), "leader.pid");
    const made = supervisor();
    const leader = made.start(
      "leader",
      ["bash", "-c", `sleep 60 & echo $! > ${pidFile}; wait`],
      "/"
    );
    const quiet = made.start("quiet", ["sleep", "60"], "/");
    const grandchild = await pidFrom(pidFile);
    expect(groupAlive(pidOf(leader))).toBe(true);
    expect(groupAlive(pidOf(quiet))).toBe(true);

    await made.stop();

    expect(await eventually(() => !groupAlive(pidOf(leader)))).toBe(true);
    expect(await eventually(() => !groupAlive(pidOf(quiet)))).toBe(true);
    expect(await eventually(() => !processAlive(grandchild))).toBe(true);
  });

  test(
    "returns as soon as the leaders exit, not after the force-kill delay",
    async () => {
      const made = supervisor();
      const quiet = made.start("quiet", ["sleep", "60"], "/");
      // The force-kill delay is far above this row's own limit (`STOP_ROW_MS`), so that limit
      // is what catches a `stop` that waits for the timer, and nothing here reads a clock. What
      // the return means is asserted below: the leader is gone by the time `stop` resolves, so a
      // `stop` that returned early having killed nothing fails here rather than passing on its
      // own silence.
      await made.stop(BEYOND_THE_ROW_MS);
      expect(groupAlive(pidOf(quiet))).toBe(false);
    },
    STOP_ROW_MS
  );

  test(
    "ends a foreground step that a stop interrupts",
    async () => {
      const pidFile = join(makeDirectory(), "step.pid");
      const made = supervisor();
      // `exec` makes the shell become the sleep, so the pid in the file is the one process of
      // this group and the test says so itself rather than resting on a bash that happens to
      // drop its own frame for the last command of a `-c` list (5.1 and later do; 3.2 does not).
      const step = made.run(["bash", "-c", `echo $$ > ${pidFile}; exec sleep 60`], "/");
      const leader = await pidFrom(pidFile);
      made.interrupt();
      await expect(step).rejects.toBeInstanceOf(Interrupted);
      // The force-kill delay is far above this row's own limit, `STOP_ROW_MS`, as in the row
      // above.
      await made.stop(BEYOND_THE_ROW_MS);
      // `run` splices the step out of the supervisor's children on any exit the child makes by
      // itself, before it throws on a non-zero code; the step stays in the list only when the
      // interrupt rejects first, as it did here. So it is `stop` that ends this process: what
      // this catches is a `stop` that returned having killed nothing.
      expect(groupAlive(leader)).toBe(false);
    },
    STOP_ROW_MS
  );
});

describe("Supervisor.untilInterrupted", () => {
  test("handles the step it was given after an interrupt, so nothing goes unhandled", async () => {
    const made = supervisor();
    made.interrupt();
    const unhandled: unknown[] = [];
    const record = (reason: unknown) => {
      unhandled.push(reason);
    };
    process.on("unhandledRejection", record);
    try {
      const { promise: step, reject } = Promise.withResolvers<never>();
      await expect(made.untilInterrupted(step)).rejects.toBeInstanceOf(Interrupted);
      reject(new Error("the step fails after the interrupt"));
      await nextTurn();
      await nextTurn();
    } finally {
      process.off("unhandledRejection", record);
    }
    expect(unhandled).toEqual([]);
  });

  test("answers the step's own result when no stop is under way", async () => {
    expect(await supervisor().untilInterrupted(Promise.resolve(7))).toBe(7);
  });
});

describe("databaseUrlRefusal", () => {
  test.each([
    "postgres://postgres@127.0.0.1:55439/dispatch_local?sslmode=disable",
    "postgres://u@LOCALHOST/d",
    "postgres://u@[::1]:5432/d",
    "postgres:///d?host=/var/run/postgresql",
    "postgresql://%2Fvar%2Frun%2Fpostgresql/d",
    "postgres:///d",
  ])("accepts %s", (url) => {
    expect(databaseUrlRefusal(url)).toBeUndefined();
  });

  test.each([
    ["postgres://u@db.example.com/x", "not db.example.com"],
    ["postgres://u@127.0.0.1/x?host=db.example.com", "not db.example.com"],
    ["postgres:///d?host=127.0.0.1,db.example.com", "not db.example.com"],
    ["postgres://u@127.0.0.1:5432,db.example.com:5432/d", "a single postgres:// URL"],
    ["host=db.example.com dbname=d", "a single postgres:// URL"],
  ])("refuses %s, saying %s", (url, said) => {
    expect(databaseUrlRefusal(url)).toContain(said);
  });
});

/** The quoting cases dev-postgres.sh's `quote` is held to as well, so the two copies of the rule
 *  cannot drift apart. */
const quoteCases = (await Bun.file(
  new URL("../../../envoy/scripts/fixtures/shell-quote.json", import.meta.url)
).json()) as { word: string; quoted: string }[];

describe("shellQuote", () => {
  test("the shared table has cases", () => {
    expect(quoteCases.length).toBeGreaterThan(0);
  });

  test.each(quoteCases)("quotes $word as $quoted, which reads back whole in bash", async (row) => {
    expect(shellQuote(row.word)).toBe(row.quoted);
    const echoed = await Bun.$`bash -c ${`printf %s ${shellQuote(row.word)}`}`.text();
    expect(echoed).toBe(row.word);
  });
});

// `agentViewRefusal` opens a real live socket, and this package's test preload replaces
// `WebSocket` with a stub for the web unit tests (`web/src/__tests__/setup.ts`), which never
// connects. So these rows run it in a child process, as `bun run dispatch:local` does, against a
// stand-in Dispatch this process serves.
const supportModule = new URL("../local-instance-support.ts", import.meta.url).pathname;

describe("agentViewRefusal", () => {
  const SESSION = "019a1c01-0d7b-7a2a-8e93-4a5b6c7d8e01";
  const servers: FakeDispatchServer<unknown>[] = [];
  afterEach(() => {
    for (const server of servers.splice(0)) server.stop();
  });

  /** The answer `agentViewRefusal` gives for `sessionId` against `baseUrl`, read from a process
   *  with the runtime's own WebSocket: its refusal, or null for none. */
  async function refusalFrom(
    baseUrl: string,
    headers: Record<string, string>,
    sessionId: string
  ): Promise<string | null> {
    const code =
      `const { agentViewRefusal } = await import(${JSON.stringify(supportModule)});\n` +
      `const answer = await agentViewRefusal(${JSON.stringify(baseUrl)}, ` +
      `${JSON.stringify(headers)}, ${JSON.stringify(sessionId)});\n` +
      "process.stdout.write(JSON.stringify(answer ?? null));\n";
    const child = Bun.spawn([process.execPath, "-e", code], { stderr: "pipe", stdout: "pipe" });
    const [code_, stdout, stderr] = await Promise.all([
      child.exited,
      new Response(child.stdout).text(),
      new Response(child.stderr).text(),
    ]);
    if (code_ !== 0) throw new Error(`the child exited ${code_}: ${stderr}`);
    return JSON.parse(stdout) as string | null;
  }

  /** A Dispatch whose live socket answers the agent view's subscribe with `answer`, recording the
   *  headers of the upgrade and every frame it was sent. */
  function dispatchAnswering(
    answer: (socket: ServerWebSocket<undefined>, sub: string) => void
  ): FakeDispatchServer<unknown> {
    const dispatch = startFakeDispatch<{ readonly sub: string }>({
      live: (socket, frame) => answer(socket, frame.sub),
    });
    servers.push(dispatch);
    return dispatch;
  }

  test("an agent view Dispatch carries is no refusal, and it subscribes as the page does", async () => {
    const dispatch = dispatchAnswering((socket, sub) =>
      socket.send(
        JSON.stringify({
          data: { responding: false, v: 1 },
          event: "responder",
          sub,
          type: "agent",
        })
      )
    );
    expect(await refusalFrom(dispatch.url, { Cookie: "dsession=alice" }, SESSION)).toBeNull();
    expect(dispatch.cookies).toEqual(["dsession=alice"]);
    expect(dispatch.received).toEqual([
      {
        sub: "local-instance-smoke",
        type: "subscribe",
        view: { kind: "agent", session_id: SESSION },
      },
    ]);
  });

  test("a session id Dispatch refuses is the refusal, named", async () => {
    const dispatch = dispatchAnswering((socket, sub) =>
      socket.send(
        JSON.stringify({
          code: "SESSION_ID_INPUT",
          message: "session_id must be a session uuid",
          sub,
          type: "error",
        })
      )
    );
    const refusal = await refusalFrom(dispatch.url, {}, "planner-session");
    expect(refusal).toContain("SESSION_ID_INPUT");
    expect(refusal).toContain("session_id must be a session uuid");
  });

  test("a socket that closes before any answer says so", async () => {
    const dispatch = dispatchAnswering((socket) => socket.close(1011, "internal error"));
    expect(await refusalFrom(dispatch.url, {}, SESSION)).toBe(
      "the live socket closed first (1011 internal error)"
    );
  });

  test("a live socket that cannot be opened at all says so", async () => {
    const dispatch = dispatchAnswering(() => undefined);
    const port = new URL(dispatch.url).port;
    for (const server of servers.splice(0)) server.stop();
    expect(await refusalFrom(`http://127.0.0.1:${port}`, {}, SESSION)).toBe(
      "the live socket could not be opened"
    );
  });
});
