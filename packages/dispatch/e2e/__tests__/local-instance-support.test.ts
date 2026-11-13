import { afterEach, beforeEach, describe, expect, jest, test } from "bun:test";
import { join } from "node:path";
import {
  type FakeDispatchServer,
  startFakeDispatch,
} from "@legion/envoy-client/test/fake-dispatch";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import type { ServerWebSocket } from "bun";

import {
  awaitSeededInbox,
  databaseUrlRefusal,
  type InboxRowRead,
  Interrupted,
  type SeededInboxRow,
  Supervisor,
  shellQuote,
} from "../local-instance-support";
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
const outlastsRow = rowLimit("short", "short", "short") / 1000;

/** Sends SIGKILL to `pid`, a process or, when negative, a process group, where it is still
 *  alive: what a stop under test left behind, so a failed row leaves no sleep running. A process
 *  the stop ended answers ESRCH, which is no failure; any other error is the row's own. */
function killLeft(pid: number): void {
  try {
    process.kill(pid, "SIGKILL");
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ESRCH") throw error;
  }
}

/** One macrotask turn: the runtime reports an unhandled rejection only after the microtask queue
 *  drains, so a test that waits for that report yields one turn rather than a guessed duration. */
const nextTurn = () => new Promise<void>((resolve) => setImmediate(resolve));

/** A `Supervisor.stop` the row started and did not await. A stop that never sees a leader exit
 *  never returns, so the row waits for `returned` with `eventually`, which fails naming the stop,
 *  rather than awaiting a promise that only the runner's clock ends. `read` runs in the stop's own
 *  settle handler, the moment the stop returns: the runtime reaps a child and raises its exit
 *  only between macrotasks, so nothing ends after the return and before `read`, and `atReturn`
 *  answers what `read` saw then. The handlers below catch every outcome, so the promise never
 *  rejects unhandled while the row fails on another line: a stop that throws counts as returned,
 *  and `forced` and `atReturn` throw what it threw. */
function underway<Read>(
  stopping: Promise<boolean>,
  read: () => Read
): {
  readonly returned: () => boolean;
  readonly forced: () => boolean;
  readonly atReturn: () => Read;
} {
  let outcome:
    | { readonly forced: boolean; readonly read: Read }
    | { readonly error: unknown }
    | undefined;
  stopping.then(
    (forced) => {
      const seen = read();
      outcome = { forced, read: seen };
    },
    (error: unknown) => {
      outcome = { error };
    }
  );
  const settled = () => {
    if (outcome === undefined) throw new Error("the stop has not returned");
    if ("error" in outcome) throw outcome.error;
    return outcome;
  };
  return {
    atReturn: () => settled().read,
    forced: () => settled().forced,
    returned: () => outcome !== undefined,
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
        const alone = grandchild;
        expect(groupAlive(leader)).toBe(true);
        expect(groupAlive(quiet)).toBe(true);

        // The force-kill fires at half the short bound, inside the wait below: a stop that leaves
        // one group to it rather than to SIGTERM returns in time and fails on `forced`. Each
        // leader is the supervisor's own child, so the runtime has reaped it by the return.
        const stop = underway(made.stop(bounds.short / 2), () => ({
          leader: processAlive(leader),
          quiet: groupAlive(quiet),
        }));
        await eventually(
          `the stop of the leader ${leader} and the quiet process ${quiet} to return`,
          stop.returned
        );
        expect(stop.forced(), "the stop's force-kill ran: SIGTERM did not end every group").toBe(
          false
        );
        const atReturn = stop.atReturn();
        expect(atReturn.leader, `the leader ${leader} outlived the stop`).toBe(false);
        expect(atReturn.quiet, `the group of the quiet process ${quiet} outlived the stop`).toBe(
          false
        );

        // The grandchild is no child of the supervisor: once its parent is gone, its zombie
        // answers `kill(pid, 0)` until init reaps it, which raises no event here and can come
        // after the return. So the leader's group, the grandchild in it, is read until it is gone.
        await eventually(
          `the group of the leader ${leader}, its grandchild ${alone} included, to end`,
          () => !groupAlive(leader) && !processAlive(alone)
        );
      } finally {
        killLeft(-leader);
        killLeft(-quiet);
        if (grandchild !== undefined) killLeft(grandchild);
      }
    },
    // Three waits the short bound of `eventually` ends, one after another: the grandchild's pid
    // in its file, the stop's return, then the leader's group leaving the process table.
    rowLimit("short", "short", "short")
  );

  test(
    "returns as soon as the leaders exit, not after the force-kill delay",
    async () => {
      const made = supervisor();
      const quiet = pidOf(made.start("quiet", ["sleep", `${outlastsRow}`], "/"));
      try {
        // The force-kill fires after the short bound, past the wait below, so a stop that waits
        // for that delay rather than for the leader to exit fails on the wait, naming the stop.
        const stop = underway(made.stop(2 * bounds.short), () => groupAlive(quiet));
        await eventually(
          `the stop of the quiet process ${quiet} to return before its force-kill`,
          stop.returned
        );
        // The force-kill cannot have run inside the wait above, so this reads only whether the
        // stop threw, which `forced()` rethrows.
        stop.forced();
        expect(stop.atReturn(), "the leader's group outlived the stop").toBe(false);
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
        const group = pid;
        const stop = underway(made.stop(bounds.short / 2), () => groupAlive(group));
        await eventually(`the stop of the interrupted step ${pid} to return`, stop.returned);
        expect(stop.forced(), "the stop's force-kill ran: SIGTERM did not end the step").toBe(
          false
        );
        expect(stop.atReturn(), "the interrupted step's group outlived the stop").toBe(false);
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

describe("awaitSeededInbox", () => {
  // Every row runs on Bun's fake timers. The helper's deadline, its sleeps and each read's budget
  // are `setTimeout`s and `Date.now()` readings, so a row moves the clock with
  // `jest.advanceTimersByTime` and decides by where the clock stands, never by how fast the
  // machine runs.
  beforeEach(() => {
    jest.useFakeTimers();
  });

  // A fake clock left behind stops the timers of every row the same worker runs after this one.
  afterEach(() => {
    jest.useRealTimers();
  });

  /** Five rows like the seed's: four of its own asks, the runbook's on a document and no issue,
   *  and the spec's decision row, declared by its block id on its issue. */
  const declared: readonly SeededInboxRow[] = [
    { askId: "ask-blocking", kind: "ask", made: "the blocking ask on CORE-2" },
    { askId: "ask-snoozed", kind: "ask", made: "the snoozed ask on CORE-3" },
    { askId: "ask-rotate", kind: "ask", made: "the rotate ask on CORE-4" },
    { askId: "ask-runbook", kind: "ask", made: "the runbook document's ask" },
    {
      blockId: "b-9f3c1a",
      issueKey: "CORE-1",
      kind: "block",
      made: "the spec decision block on CORE-1",
    },
  ];
  const decisionRow: InboxRowRead = {
    block_id: "b-9f3c1a",
    id: "ask-decision",
    issue_key: "CORE-1",
  };
  /** The four rows ahead of the decision row, which is the one a short set lacks. */
  const shortSet: readonly InboxRowRead[] = [
    { block_id: null, id: "ask-blocking", issue_key: "CORE-2" },
    { block_id: null, id: "ask-snoozed", issue_key: "CORE-3" },
    { block_id: null, id: "ask-rotate", issue_key: "CORE-4" },
    {
      block_id: null,
      document: { name: "Runbook", project: "CORE", slug: "runbook" },
      id: "ask-runbook",
      issue_key: null,
    },
  ];
  const wholeSet: readonly InboxRowRead[] = [...shortSet, decisionRow];
  const missingDecision = "    - the spec decision block on CORE-1 (block b-9f3c1a)";
  const owner =
    "  These rows are written by the seed (packages/dispatch/e2e/workspace.ts) and by Dispatch's " +
    "own document settlement, not by the change under test.";
  /** For every row that waits out its deadline: 300 ms on the moved clock. */
  const quick = { deadlineMs: 300, intervalMs: 20 };

  /** A reader that answers `answer(call)` for its 1-based call, or throws it when it is an error,
   *  and counts its calls. */
  function reader(answer: (call: number) => readonly InboxRowRead[] | Error) {
    const made = {
      calls: 0,
      read: async (): Promise<readonly InboxRowRead[]> => {
        made.calls += 1;
        const answered = answer(made.calls);
        if (answered instanceof Error) throw answered;
        return answered;
      },
    };
    return made;
  }

  /** Lets every promise the helper chained run as far as it goes without the clock moving. */
  async function settle(): Promise<void> {
    for (let turn = 0; turn < 30; turn += 1) await Promise.resolve();
  }

  /** Answers whether `waiting` has settled yet, without awaiting it. */
  function watch(waiting: Promise<unknown>): () => boolean {
    let settled = false;
    const done = () => {
      settled = true;
    };
    waiting.then(done, done);
    return () => settled;
  }

  /** Moves the fake clock `ms` on one millisecond at a time, and lets what each step released run
   *  before the next, so a sleep or a read the helper starts inside the span is timed from the
   *  moment it starts. One jump is not enough: once the process has spawned a child, Bun fires
   *  every timer of the span before the helper's continuations run, and the next sleep then
   *  starts at the end of the span. */
  async function pass(ms: number): Promise<void> {
    await settle();
    for (let step = 0; step < ms; step += 1) {
      jest.advanceTimersByTime(1);
      await settle();
    }
  }

  /** Moves the clock `ms` on and checks that `waiting` has settled by then, so a wait that never
   *  ends fails on this line rather than on the runner's timeout. */
  async function settledAfter(ms: number, waiting: Promise<unknown>): Promise<void> {
    const settled = watch(waiting);
    await pass(ms);
    expect(settled()).toBe(true);
  }

  /** The message `waiting` rejects with, which must be an Error ending in the owner line. */
  async function failureOf(waiting: Promise<unknown>): Promise<string> {
    const error = await waiting.then(
      () => undefined,
      (reason: unknown) => reason
    );
    expect(error).toBeInstanceOf(Error);
    const { message } = error as Error;
    expect(message.split("\n").at(-1)).toBe(owner);
    return message;
  }

  test("a row that arrives late is waited for", async () => {
    const inbox = reader((call) => (call < 3 ? shortSet : wholeSet));
    const waiting = awaitSeededInbox(declared, inbox.read, { deadlineMs: 10_000, intervalMs: 20 });
    // Reads at 0, 20 and 40 ms.
    await settledAfter(40, waiting);
    const match = await waiting;
    expect(match.reads).toBe(3);
    expect(match.line).toBe("inbox rows: 5 of 5 declared rows matched in 3 reads");
  });

  test("the right number of rows with one not declared names it and the absent row", async () => {
    // The stray row carries the declared block id on another issue, so it is not the decision row.
    const stray: InboxRowRead = { block_id: "b-9f3c1a", id: "ask-stray", issue_key: "CORE-7" };
    const inbox = reader(() => [...shortSet, stray]);
    const waiting = awaitSeededInbox(declared, inbox.read, quick);
    await settledAfter(quick.deadlineMs, waiting);
    const message = await failureOf(waiting);
    expect(message).toContain(
      [
        "  missing (the seed declares it, the Inbox did not list it):",
        missingDecision,
        "  unexpected (the Inbox listed it, the seed declares no such row):",
        "    - ask ask-stray on CORE-7 (block b-9f3c1a)",
        "  the Inbox answered 5 rows; the seed declares 5.",
      ].join("\n")
    );
  });

  test("every declared row and one more names the extra row and no missing row", async () => {
    const extra: InboxRowRead = { block_id: null, id: "ask-extra", issue_key: "CORE-9" };
    const inbox = reader(() => [...wholeSet, extra]);
    const waiting = awaitSeededInbox(declared, inbox.read, quick);
    await settledAfter(quick.deadlineMs, waiting);
    const message = await failureOf(waiting);
    expect(message).toContain(
      [
        "  missing (the seed declares it, the Inbox did not list it):",
        "    - none",
        "  unexpected (the Inbox listed it, the seed declares no such row):",
        "    - ask ask-extra on CORE-9 (block none)",
        "  the Inbox answered 6 rows; the seed declares 5.",
      ].join("\n")
    );
  });

  test("a row that never arrives is named missing after the deadline", async () => {
    const inbox = reader(() => shortSet);
    const started = Date.now();
    const waiting = awaitSeededInbox(declared, inbox.read, quick);
    const settled = watch(waiting);
    await pass(quick.deadlineMs - 1);
    expect(settled()).toBe(false);
    await pass(1);
    expect(settled()).toBe(true);
    const message = await failureOf(waiting);
    // Both readings are of the moved clock: the row's own and the wait the helper reports.
    expect(Date.now() - started).toBeGreaterThanOrEqual(300);
    const waited = message.match(
      new RegExp(
        `^inbox rows: the Inbox and the seed still disagree after (\\d+\\.\\d) s and ${inbox.calls} reads\\.\\n`
      )
    );
    expect(waited).not.toBeNull();
    expect(Number(waited?.[1])).toBeGreaterThanOrEqual(0.3);
    expect(message).toContain(
      [
        "  missing (the seed declares it, the Inbox did not list it):",
        missingDecision,
        "  unexpected (the Inbox listed it, the seed declares no such row):",
        "    - none",
        "  the Inbox answered 4 rows; the seed declares 5.",
      ].join("\n")
    );
    expect(message).not.toContain("the last read failed");
  });

  test("a wait given no options ends at the 15 s default deadline", async () => {
    // `e2e/workspace.e2e.ts` passes no options, so this deadline is the one its wait has.
    const inbox = reader(() => shortSet);
    const waiting = awaitSeededInbox(declared, inbox.read);
    const settled = watch(waiting);
    await pass(15_000 - 1);
    expect(settled()).toBe(false);
    await pass(1);
    expect(settled()).toBe(true);
    const message = await failureOf(waiting);
    expect(message.split("\n")[0]).toBe(
      `inbox rows: the Inbox and the seed still disagree after 15.0 s and ${inbox.calls} reads.`
    );
  });

  test("the whole set at once returns on the first read", async () => {
    const inbox = reader(() => wholeSet);
    const waiting = awaitSeededInbox(declared, inbox.read);
    await settledAfter(0, waiting);
    const match = await waiting;
    expect(inbox.calls).toBe(1);
    expect(match.reads).toBe(1);
    expect(match.line).toBe("inbox rows: 5 of 5 declared rows matched in 1 read");
  });

  test("an abort while a read hangs rejects with Interrupted at once", async () => {
    const asked = Promise.withResolvers<void>();
    const stopping = new AbortController();
    const waiting = awaitSeededInbox(
      declared,
      () => {
        asked.resolve();
        return new Promise<never>(() => {});
      },
      { deadlineMs: 10_000, intervalMs: 20, signal: stopping.signal }
    );
    await asked.promise;
    // The clock does not move after the abort: "at once" is before any timer fires.
    stopping.abort();
    await settledAfter(0, waiting);
    await expect(waiting).rejects.toBeInstanceOf(Interrupted);
  });

  test("reads that fail first and then answer the whole set return", async () => {
    const inbox = reader((call) =>
      call <= 3 ? new Error("GET /api/v1/inbox failed: 502 bad gateway") : wholeSet
    );
    const waiting = awaitSeededInbox(declared, inbox.read, { deadlineMs: 10_000, intervalMs: 20 });
    // Reads at 0, 20, 40 and 60 ms.
    await settledAfter(60, waiting);
    const match = await waiting;
    expect(inbox.calls).toBe(4);
    expect(match.reads).toBe(inbox.calls);
    expect(match.line).toBe(`inbox rows: 5 of 5 declared rows matched in ${inbox.calls} reads`);
  });

  test("reads that always fail say that no read answered, and the last error", async () => {
    const inbox = reader((call) => new Error(`GET /api/v1/inbox failed: 500 attempt ${call}`));
    const waiting = awaitSeededInbox(declared, inbox.read, quick);
    await settledAfter(quick.deadlineMs, waiting);
    const message = await failureOf(waiting);
    const lines = message.split("\n");
    expect(lines[0]).toMatch(
      new RegExp(
        `^inbox rows: no read of the Inbox answered in \\d+\\.\\d s over ${inbox.calls} reads\\.$`
      )
    );
    expect(lines.slice(1)).toEqual([
      "  the seed declares 5 rows.",
      `  the last read failed: GET /api/v1/inbox failed: 500 attempt ${inbox.calls}`,
      owner,
    ]);
  });

  test("a second copy of a declared row is unexpected, and no row is missing", async () => {
    const inbox = reader(() => [...wholeSet, shortSet[1]]);
    const waiting = awaitSeededInbox(declared, inbox.read, quick);
    await settledAfter(quick.deadlineMs, waiting);
    const message = await failureOf(waiting);
    expect(message).toContain(
      [
        "  missing (the seed declares it, the Inbox did not list it):",
        "    - none",
        "  unexpected (the Inbox listed it, the seed declares no such row):",
        "    - ask ask-snoozed on CORE-3 (block none)",
        "  the Inbox answered 6 rows; the seed declares 5.",
      ].join("\n")
    );
  });

  test("a short set and then failed reads name the missing row and the last error", async () => {
    const inbox = reader((call) =>
      call === 1 ? shortSet : new Error(`GET /api/v1/inbox failed: 500 attempt ${call}`)
    );
    const waiting = awaitSeededInbox(declared, inbox.read, quick);
    await settledAfter(quick.deadlineMs, waiting);
    const message = await failureOf(waiting);
    expect(message).toStartWith("inbox rows: the Inbox and the seed still disagree after ");
    expect(message).toContain(
      [
        "  missing (the seed declares it, the Inbox did not list it):",
        missingDecision,
        "  unexpected (the Inbox listed it, the seed declares no such row):",
        "    - none",
        "  the Inbox answered 4 rows; the seed declares 5.",
        `  the last read failed: GET /api/v1/inbox failed: 500 attempt ${inbox.calls}`,
        owner,
      ].join("\n")
    );
  });

  test("a read that never answers is a failed read once the deadline passes", async () => {
    const waiting = awaitSeededInbox(declared, () => new Promise<never>(() => {}), quick);
    const settled = watch(waiting);
    await pass(quick.deadlineMs - 1);
    expect(settled()).toBe(false);
    await pass(1);
    expect(settled()).toBe(true);
    const message = await failureOf(waiting);
    expect(message.split("\n").slice(1)).toEqual([
      "  the seed declares 5 rows.",
      "  the last read failed: the read had not answered within 0.3 s",
      owner,
    ]);
  });

  test("a read that hangs near the deadline holds the wait one interval past it at most", async () => {
    // Short sets answer at once for the first 200 ms; the read that starts after that never
    // answers, and may take only the time the deadline leaves it, not a whole deadline.
    const started = Date.now();
    let calls = 0;
    const read = (): Promise<readonly InboxRowRead[]> => {
      calls += 1;
      return Date.now() - started < 200 ? Promise.resolve(shortSet) : new Promise<never>(() => {});
    };
    const waiting = awaitSeededInbox(declared, read, quick);
    const settled = watch(waiting);
    await pass(quick.deadlineMs - 1);
    expect(settled()).toBe(false);
    await pass(1 + quick.intervalMs);
    expect(settled()).toBe(true);
    const message = await failureOf(waiting);
    expect(calls).toBeGreaterThan(1);
    const lines = message.split("\n");
    expect(lines[0]).toMatch(
      new RegExp(
        `^inbox rows: the Inbox and the seed still disagree after \\d+\\.\\d s and ${calls} reads\\.$`
      )
    );
    expect(lines.slice(1, -2)).toEqual([
      "  missing (the seed declares it, the Inbox did not list it):",
      missingDecision,
      "  unexpected (the Inbox listed it, the seed declares no such row):",
      "    - none",
      "  the Inbox answered 4 rows; the seed declares 5.",
    ]);
    expect(lines.at(-2)).toMatch(
      /^ {2}the last read failed: the read had not answered within 0\.\d s$/
    );
    expect(lines.at(-1)).toBe(owner);
  });

  test("the wait ends at the deadline and starts no read after it", async () => {
    // Reads at 0 ms and at 200 ms; the second sleep is cut to the 100 ms the deadline leaves, so
    // no third read starts at 400 ms with no time left to answer in.
    const inbox = reader(() => shortSet);
    const waiting = awaitSeededInbox(declared, inbox.read, { deadlineMs: 300, intervalMs: 200 });
    const settled = watch(waiting);
    await pass(299);
    expect(settled()).toBe(false);
    await pass(1);
    expect(settled()).toBe(true);
    const message = await failureOf(waiting);
    expect(inbox.calls).toBeLessThanOrEqual(2);
    expect(message.split("\n")[0]).toMatch(/ and [12] reads?\.$/);
    expect(message).not.toContain("the last read failed");
  });

  test("an unexpected row on a document is named by the document", async () => {
    const stray: InboxRowRead = {
      block_id: null,
      document: { name: "Notes", project: "CORE", slug: "notes" },
      id: "ask-notes",
      issue_key: null,
    };
    const inbox = reader(() => [...wholeSet, stray]);
    const waiting = awaitSeededInbox(declared, inbox.read, quick);
    await settledAfter(quick.deadlineMs, waiting);
    const message = await failureOf(waiting);
    expect(message).toContain("    - ask ask-notes on the document Notes (block none)\n");
  });

  test("an abort while it sleeps between reads rejects with Interrupted at once", async () => {
    const stopping = new AbortController();
    const asked = Promise.withResolvers<void>();
    const inbox = reader(() => {
      asked.resolve();
      return shortSet;
    });
    const waiting = awaitSeededInbox(declared, inbox.read, {
      deadlineMs: 10_000,
      intervalMs: 5_000,
      signal: stopping.signal,
    });
    // The first read has answered, and once its promises have run the helper is in its 5 s
    // sleep. The clock does not move after the abort: "at once" is before any timer fires.
    await asked.promise;
    await settle();
    stopping.abort();
    await settledAfter(0, waiting);
    await expect(waiting).rejects.toBeInstanceOf(Interrupted);
    expect(inbox.calls).toBe(1);
  });
});
