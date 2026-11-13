import { afterEach, describe, expect, test } from "bun:test";
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

  test("returns as soon as the leaders exit, not after the force-kill delay", async () => {
    const made = supervisor();
    made.start("quiet", ["sleep", "60"], "/");
    const started = Date.now();
    await made.stop(30_000);
    expect(Date.now() - started).toBeLessThan(5_000);
  });

  test("ends a foreground step that a stop interrupts", async () => {
    const made = supervisor();
    const step = made.run(["sleep", "60"], "/");
    made.interrupt();
    await expect(step).rejects.toBeInstanceOf(Interrupted);
    const started = Date.now();
    await made.stop();
    expect(Date.now() - started).toBeLessThan(5_000);
  });
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

describe("awaitSeededInbox", () => {
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
    { id: "ask-rotate", issue_key: "CORE-4" },
    { document: { name: "Runbook", slug: "runbook" }, id: "ask-runbook", issue_key: null },
  ];
  const wholeSet: readonly InboxRowRead[] = [...shortSet, decisionRow];
  const missingDecision = "    - the spec decision block on CORE-1 (block b-9f3c1a)";
  const owner =
    "  These rows are written by the seed (packages/dispatch/e2e/workspace.ts) and by Dispatch's " +
    "own document settlement, not by the change under test.";
  /** For every row that waits out its deadline, so none costs the 15 second default. */
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
    const match = await awaitSeededInbox(declared, inbox.read, {
      deadlineMs: 10_000,
      intervalMs: 20,
    });
    expect(match.reads).toBe(3);
    expect(match.matched).toBe(5);
    expect(match.line).toBe("inbox rows: 5 of 5 declared rows matched in 3 reads");
  });

  test("the right number of rows with one not declared names it and the absent row", async () => {
    // The stray row carries the declared block id on another issue, so it is not the decision row.
    const stray: InboxRowRead = { block_id: "b-9f3c1a", id: "ask-stray", issue_key: "CORE-7" };
    const inbox = reader(() => [...shortSet, stray]);
    const message = await failureOf(awaitSeededInbox(declared, inbox.read, quick));
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
    const inbox = reader(() => [...wholeSet, { id: "ask-extra", issue_key: "CORE-9" }]);
    const message = await failureOf(awaitSeededInbox(declared, inbox.read, quick));
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
    const message = await failureOf(awaitSeededInbox(declared, inbox.read, quick));
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

  test("the whole set at once returns on the first read", async () => {
    const inbox = reader(() => wholeSet);
    const match = await awaitSeededInbox(declared, inbox.read);
    expect(inbox.calls).toBe(1);
    expect(match.reads).toBe(1);
    expect(match.line).toBe("inbox rows: 5 of 5 declared rows matched in 1 read");
    expect(match.line.endsWith("in 1 read")).toBe(true);
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
    const aborted = Date.now();
    stopping.abort();
    await expect(waiting).rejects.toBeInstanceOf(Interrupted);
    expect(Date.now() - aborted).toBeLessThan(1_000);
  });

  test("reads that fail first and then answer the whole set return", async () => {
    const inbox = reader((call) =>
      call <= 3 ? new Error("GET /api/v1/inbox failed: 502 bad gateway") : wholeSet
    );
    const match = await awaitSeededInbox(declared, inbox.read, {
      deadlineMs: 10_000,
      intervalMs: 20,
    });
    expect(inbox.calls).toBe(4);
    expect(match.reads).toBe(inbox.calls);
    expect(match.line).toBe(`inbox rows: 5 of 5 declared rows matched in ${inbox.calls} reads`);
  });

  test("reads that always fail say that no read answered, and the last error", async () => {
    const inbox = reader((call) => new Error(`GET /api/v1/inbox failed: 500 attempt ${call}`));
    const message = await failureOf(awaitSeededInbox(declared, inbox.read, quick));
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
    const message = await failureOf(awaitSeededInbox(declared, inbox.read, quick));
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
    const message = await failureOf(awaitSeededInbox(declared, inbox.read, quick));
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

  test("a read that never answers ends the wait at the deadline", async () => {
    const started = Date.now();
    const message = await failureOf(
      awaitSeededInbox(declared, () => new Promise<never>(() => {}), quick)
    );
    expect(Date.now() - started).toBeLessThan(2_000);
    expect(message.split("\n").slice(1)).toEqual([
      "  the seed declares 5 rows.",
      "  the last read failed: the read had not answered when the 0.3 s deadline passed",
      owner,
    ]);
  });

  test("an unexpected row on a document is named by the document", async () => {
    const stray: InboxRowRead = { document: { name: "Notes", slug: "notes" }, id: "ask-notes" };
    const inbox = reader(() => [...wholeSet, stray]);
    const message = await failureOf(awaitSeededInbox(declared, inbox.read, quick));
    expect(message).toContain("    - ask ask-notes on the document Notes (block none)\n");
  });

  test("an abort while it sleeps between reads rejects with Interrupted at once", async () => {
    const stopping = new AbortController();
    const asked = Promise.withResolvers<void>();
    const inbox = reader(() => {
      asked.resolve();
      return shortSet;
    });
    const started = Date.now();
    const waiting = awaitSeededInbox(declared, inbox.read, {
      deadlineMs: 10_000,
      intervalMs: 5_000,
      signal: stopping.signal,
    });
    // The first read has answered, and one macrotask turn later the helper is in its 5 s sleep.
    await asked.promise;
    await nextTurn();
    stopping.abort();
    await expect(waiting).rejects.toBeInstanceOf(Interrupted);
    expect(Date.now() - started).toBeLessThan(1_000);
    expect(inbox.calls).toBe(1);
  });
});
