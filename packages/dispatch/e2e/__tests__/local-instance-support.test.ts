import { afterEach, describe, expect, test } from "bun:test";
import { join } from "node:path";
import {
  type FakeDispatchServer,
  startFakeDispatch,
} from "@legion/envoy-client/test/fake-dispatch";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import type { ServerWebSocket } from "bun";

import { databaseUrlRefusal, Interrupted, Supervisor, shellQuote } from "../local-instance-support";
import { bounds, eventually, rowLimit, runBounded } from "./bounded-subprocess";

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

/** The pid a real child process writes to `path`. A write another process makes raises no event
 *  here, so the file is read until it holds one. */
async function pidFrom(path: string): Promise<number> {
  const read = () =>
    Bun.file(path)
      .text()
      .catch(() => "");
  await eventually(`a pid in ${path}`, async () => /^\d+\s*$/.test(await read()));
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

/** How long each `sleep` the `Supervisor.stop` rows start runs, in seconds: the limit of the
 *  first of them, the largest of the three, so every such sleep outlasts its own row. A sleep no
 *  stop ended is then still running at every read its row makes, and never meets a check that it
 *  is gone by ending on its own. */
const outlastsRow = rowLimit("short", "short", "short", "short", "short") / 1000;

/** Sends SIGKILL to `pid`, a process or, when negative, a process group, where it is still
 *  alive: what a stop under test left behind, so a failed row leaves no sleep running. */
function killLeft(pid: number): void {
  try {
    process.kill(pid, "SIGKILL");
  } catch {
    // ESRCH: the stop ended it, as it should.
  }
}

/** One macrotask turn: the runtime reports an unhandled rejection only after the microtask queue
 *  drains, so a test that waits for that report yields one turn rather than a guessed duration. */
const nextTurn = () => new Promise<void>((resolve) => setImmediate(resolve));

/** A `Supervisor.stop` the row started and did not await. A stop that never sees a leader exit
 *  never returns, so the row waits for `returned` with `eventually`, which fails naming the stop,
 *  rather than awaiting a promise that only the runner's clock ends. The handlers below catch
 *  every outcome, so the promise never rejects unhandled while the row fails on another line:
 *  a stop that throws counts as returned, and `forced` throws what it threw. */
function underway(stopping: Promise<boolean>): {
  readonly returned: () => boolean;
  readonly forced: () => boolean;
} {
  let outcome: { readonly forced: boolean } | { readonly error: unknown } | undefined;
  stopping.then(
    (forced) => {
      outcome = { forced };
    },
    (error: unknown) => {
      outcome = { error };
    }
  );
  return {
    returned: () => outcome !== undefined,
    forced: () => {
      if (outcome === undefined) throw new Error("the stop has not returned");
      if ("error" in outcome) throw outcome.error;
      return outcome.forced;
    },
  };
}

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
  test(
    "ends every process group it started, a grandchild included",
    async () => {
      const pidFile = join(makeDirectory(), "leader.pid");
      const made = supervisor();
      const leader = pidOf(
        made.start(
          "leader",
          ["bash", "-c", `sleep ${outlastsRow} & echo $! > ${pidFile}; wait`],
          "/"
        )
      );
      const quiet = pidOf(made.start("quiet", ["sleep", `${outlastsRow}`], "/"));
      let grandchild: number | undefined;
      try {
        grandchild = await pidFrom(pidFile);
        expect(groupAlive(leader)).toBe(true);
        expect(groupAlive(quiet)).toBe(true);

        // The force-kill fires at half the short bound, inside the wait below: a stop that leaves
        // one group to it rather than to SIGTERM returns in time and fails on `forced`.
        const stop = underway(made.stop(30_000));
        await eventually(
          `the stop of the leader ${leader} and the quiet process ${quiet} to return`,
          stop.returned
        );
        expect(stop.forced(), "the stop's force-kill ran: SIGTERM did not end every group").toBe(
          false
        );

        // A killed process still answers `kill(pid, 0)` as a zombie until its parent or init
        // reaps it, which raises no event here, so each is read until it is gone.
        await eventually(`the group of the leader ${leader} to end`, () => !groupAlive(leader));
        await eventually(
          `the group of the quiet process ${quiet} to end`,
          () => !groupAlive(quiet)
        );
        const alone = grandchild;
        await eventually(`the grandchild ${alone} to end`, () => !processAlive(alone));
      } finally {
        killLeft(-leader);
        killLeft(-quiet);
        if (grandchild !== undefined) killLeft(grandchild);
      }
    },
    // Five waits this row ends at a bound itself, one after another: the grandchild's pid in its
    // file, the stop's return, then the two groups and the grandchild leaving the process table.
    rowLimit("short", "short", "short", "short", "short")
  );

  test(
    "returns as soon as the leaders exit, not after the force-kill delay",
    async () => {
      const made = supervisor();
      const quiet = pidOf(made.start("quiet", ["sleep", `${outlastsRow}`], "/"));
      try {
        // The force-kill fires after the short bound, past the wait below, so a stop that waits
        // for that delay rather than for the leader to exit fails on the wait, naming the stop.
        const stop = underway(made.stop(2 * bounds.short));
        await eventually(
          `the stop of the quiet process ${quiet} to return before its force-kill`,
          stop.returned
        );
        // The force-kill cannot have run inside the wait above, so this reads only whether the
        // stop threw, which `forced()` rethrows.
        stop.forced();
        expect(groupAlive(quiet), "the leader's group outlived the stop").toBe(false);
      } finally {
        killLeft(-quiet);
      }
    },
    // The wait for the stop's return, which the short bound of `eventually` ends.
    rowLimit("short")
  );

  test(
    "ends a foreground step that a stop interrupts",
    async () => {
      const pidFile = join(makeDirectory(), "step.pid");
      const made = supervisor();
      const step = made.run(["bash", "-c", `echo $$ > ${pidFile}; exec sleep ${outlastsRow}`], "/");
      let pid: number | undefined;
      try {
        pid = await pidFrom(pidFile);
        made.interrupt();
        await expect(step).rejects.toBeInstanceOf(Interrupted);
        // The force-kill fires at half the short bound, inside the wait below: a stop that leaves
        // the step to it rather than to SIGTERM returns in time and fails on `forced`.
        const stop = underway(made.stop(30_000));
        await eventually(`the stop of the interrupted step ${pid} to return`, stop.returned);
        expect(stop.forced(), "the stop's force-kill ran: SIGTERM did not end the step").toBe(
          false
        );
        expect(groupAlive(pid), "the interrupted step's group outlived the stop").toBe(false);
      } finally {
        if (pid !== undefined) killLeft(-pid);
      }
    },
    // Two waits the short bound of `eventually` ends, one after the other: the step's pid in its
    // file, then the stop's return.
    rowLimit("short", "short")
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

  test.each(quoteCases)(
    "quotes $word as $quoted, which reads back whole in bash",
    async (row) => {
      expect(shellQuote(row.word)).toBe(row.quoted);
      const { code, named, stderr, stdout } = await runBounded({
        argv: ["bash", "-c", `printf %s ${shellQuote(row.word)}`],
        bound: "short",
        from: import.meta.url,
      });
      if (code !== 0) throw new Error(`${named} exited ${code}: ${stderr}`);
      expect(stdout).toBe(row.word);
    },
    rowLimit("short")
  );
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
   *  with the runtime's own WebSocket: its refusal, or null for none. The first frame gets the
   *  short bound, which the child's own bound ends too, so no row reads a stall of this machine
   *  as Dispatch's silence. */
  async function refusalFrom(
    baseUrl: string,
    headers: Record<string, string>,
    sessionId: string
  ): Promise<string | null> {
    const script =
      `const { agentViewRefusal } = await import(${JSON.stringify(supportModule)});\n` +
      `const answer = await agentViewRefusal(${JSON.stringify(baseUrl)}, ` +
      `${JSON.stringify(headers)}, ${JSON.stringify(sessionId)}, ${bounds.short});\n` +
      "process.stdout.write(JSON.stringify(answer ?? null));\n";
    const { code, named, stderr, stdout } = await runBounded({
      argv: [process.execPath, "-e", script],
      bound: "short",
      from: import.meta.url,
    });
    if (code !== 0) throw new Error(`the child ${named} exited ${code}: ${stderr}`);
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

  test(
    "an agent view Dispatch carries is no refusal, and it subscribes as the page does",
    async () => {
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
    },
    rowLimit("short")
  );

  test(
    "a session id Dispatch refuses is the refusal, named",
    async () => {
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
    },
    rowLimit("short")
  );

  test(
    "a socket that closes before any answer says so",
    async () => {
      const dispatch = dispatchAnswering((socket) => socket.close(1011, "internal error"));
      expect(await refusalFrom(dispatch.url, {}, SESSION)).toBe(
        "the live socket closed first (1011 internal error)"
      );
    },
    rowLimit("short")
  );

  test(
    "a live socket that cannot be opened at all says so",
    async () => {
      const dispatch = dispatchAnswering(() => undefined);
      const port = new URL(dispatch.url).port;
      for (const server of servers.splice(0)) server.stop();
      expect(await refusalFrom(`http://127.0.0.1:${port}`, {}, SESSION)).toBe(
        "the live socket could not be opened"
      );
    },
    rowLimit("short")
  );
});
