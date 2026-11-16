/** What `e2e/local-instance.ts` and `e2e/harness-start.ts` run on: the processes they start and
 *  stop, the refusal of a database the command must not truncate, and the quoting of the commands
 *  it prints; and the check that the Inbox lists the rows `e2e/workspace.ts` seeded
 *  (`awaitSeededInbox`), which the command and `e2e/workspace.e2e.ts` both run. Nothing here runs
 *  on import, so `e2e/__tests__/local-instance-support.test.ts` drives it without a server. */
import { type ChildProcess, spawn } from "node:child_process";
import type { Readable } from "node:stream";
import { setTimeout as sleep } from "node:timers/promises";
import type { InboxRow } from "../web/src/api/types";

export class Interrupted extends Error {}

/** `step`, or Interrupted as soon as `signal` aborts; `step` itself with no signal. `step`'s own
 *  outcome is always handled, so a step the abort outruns settles later into nothing rather than
 *  an unhandled rejection. */
function untilAborted<T>(step: Promise<T>, signal: AbortSignal | undefined): Promise<T> {
  if (signal === undefined) return step;
  const { promise, resolve, reject } = Promise.withResolvers<T>();
  const onAbort = () => reject(new Interrupted("interrupted"));
  signal.addEventListener("abort", onAbort, { once: true });
  step.then(resolve, reject).finally(() => signal.removeEventListener("abort", onAbort));
  if (signal.aborted) onAbort();
  return promise;
}

interface Running {
  readonly name: string;
  readonly child: ChildProcess;
  /** Settles with the child's exit code, or with null when a signal ended it, and rejects when
   *  the process could not be started at all. Node's `ChildProcess` has no promise of its own,
   *  and a listener attached after the exit would never fire, so it is made here, the moment the
   *  process starts. */
  readonly exited: Promise<number | null>;
}

/** A process started by `startPiped`: its output is the caller's to read. */
export interface PipedRunning extends Running {
  readonly child: ChildProcess & { readonly stdout: Readable; readonly stderr: Readable };
}

/** Every process the command starts, as the leader of a process group of its own, and the one
 *  signal that a stop is under way: a Ctrl-C (`interrupt`) or the end of the run (`stop`). */
export class Supervisor {
  readonly #children: Running[] = [];
  readonly #stopping = new AbortController();

  /** Aborted by `interrupt` or `stop`. A step that starts a process outside `start` (psql through
   *  e2e/psql.ts) passes it on, so the process ends with the others. */
  get signal(): AbortSignal {
    return this.#stopping.signal;
  }

  interrupt(): void {
    this.#stopping.abort();
  }

  /** `step`, or Interrupted as soon as a stop is under way (`untilAborted`); `stop` ends the
   *  processes it waits on. */
  untilInterrupted<T>(step: Promise<T>): Promise<T> {
    return untilAborted(step, this.#stopping.signal);
  }

  /** Starts `argv` as the leader of a process group of its own, so stopping it reaches what it
   *  starts in turn: run-server.sh's `flock … go build` before it execs the server, and the vite
   *  process `bun run build:web` runs. A terminal's Ctrl-C reaches only the command, which stops
   *  them in order. Their output is the command's. */
  start(name: string, argv: readonly string[], cwd: string): Running {
    return this.#spawn(name, argv, cwd, process.env, "inherit");
  }

  /** As `start`, in the environment `env`, with its output piped to the caller, which reads both
   *  streams to their end: a pipe nobody drains stops the process once it fills. */
  startPiped(
    name: string,
    argv: readonly string[],
    cwd: string,
    env: Record<string, string | undefined>
  ): PipedRunning {
    // `spawn` types `stdout` and `stderr` as nullable, since what they are depends on the stdio
    // it was given; with "pipe" for both, the process always has the two streams.
    return this.#spawn(name, argv, cwd, env, "pipe") as PipedRunning;
  }

  /** Spawns `argv` detached, with no stdin and both output streams `output`, and tracks it. The
   *  spawn is `node:child_process`'s rather than Bun's, because this runs under both runtimes:
   *  `bun run dispatch:local` and the docs media harness are bun's, and Playwright's own runner,
   *  which starts the harness from `globalSetup` (`e2e/harness-pool.ts`), is node's. */
  #spawn<const Output extends "inherit" | "pipe">(
    name: string,
    argv: readonly string[],
    cwd: string,
    env: Record<string, string | undefined>,
    output: Output
  ) {
    const [command, ...rest] = argv;
    const child = spawn(command, rest, {
      cwd,
      detached: true,
      env,
      stdio: ["ignore", output, output],
    });
    const ended = Promise.withResolvers<number | null>();
    child.once("exit", (code) => ended.resolve(code));
    // A command that cannot be started at all - no such binary - never exits, since it never
    // ran; `error` is where Node says so, and the rejection is the caller's to see wherever it
    // awaits this process. The handler below keeps that rejection from being reported as
    // unhandled before then.
    child.once("error", (error) => ended.reject(error));
    ended.promise.catch(() => {});
    const running = { child, exited: ended.promise, name };
    this.#children.push(running);
    return running;
  }

  /** A foreground step. It throws on a non-zero exit, so a failed web build or dev-postgres.sh
   *  reaches the command's one catch (and `stop`) like every other failure. */
  async run(argv: readonly string[], cwd: string): Promise<void> {
    const step = this.start(argv.join(" "), argv, cwd);
    const code = await this.untilInterrupted(step.exited);
    this.#children.splice(this.#children.indexOf(step), 1);
    if (code !== 0) throw new Error(`${step.name} exited ${code}`);
  }

  /** Rejects as soon as any process started so far exits by itself, naming it. */
  firstExit(): Promise<never> {
    return Promise.race(
      this.#children.map(async ({ child, exited, name }): Promise<never> => {
        const code = await exited;
        throw new Error(`${name} exited ${code ?? child.signalCode} while the harness was up`);
      })
    );
  }

  /** Aborts `signal`, sends SIGTERM to every process group started and not yet seen to stop,
   *  SIGKILL to whatever still runs `forceAfterMs` later, and returns the moment every leader has
   *  exited: the force-kill timer is cleared then, so a clean Ctrl-C does not keep the shell
   *  prompt waiting on it. */
  async stop(forceAfterMs = 10_000): Promise<void> {
    this.#stopping.abort();
    const running = this.#children.splice(0);
    const signalGroups = (signal: NodeJS.Signals) => {
      for (const { child } of running) {
        // A process that never started has no pid, and there is no group to signal.
        if (child.pid === undefined) continue;
        try {
          process.kill(-child.pid, signal);
        } catch {
          // ESRCH: every process of that group has already exited.
        }
      }
    };
    signalGroups("SIGTERM");
    const forceKill = setTimeout(() => signalGroups("SIGKILL"), forceAfterMs);
    await Promise.allSettled(running.map(({ exited }) => exited));
    clearTimeout(forceKill);
  }
}

const loopbackHosts = ["127.0.0.1", "localhost", "[::1]", "::1"];

/** Why `raw` may not be the database this command truncates, or undefined when it names this
 *  machine: every host libpq would connect to is a loopback name or address or a Unix socket
 *  directory. The host is the `host` query parameter when the URL has one, as for libpq, else the
 *  URL's own (percent-decoded, so `%2Fvar%2Frun%2Fpostgresql` is a socket directory); an empty
 *  one is libpq's default socket. A key=value string is not a URL and is refused.
 *
 *  This check is the command's early refusal, not its boundary. It reads the URL with the URL
 *  parser, not libpq's; what it misses is closed by ORDER: the Dispatch server's own pgx check
 *  (cmd/dispatch loopbackDatabase) refuses to boot on a non-loopback host, and resetDatabase runs
 *  only after /healthz answered, so a URL this check wrongly approves truncates nothing. Keep the
 *  health wait ahead of the reset. */
export function databaseUrlRefusal(raw: string): string | undefined {
  let named: string;
  try {
    const url = new URL(raw);
    named = url.searchParams.get("host") ?? decodeURIComponent(url.hostname);
  } catch {
    return "DATABASE_URL must be a single postgres:// URL naming a loopback host or a Unix socket: this command truncates it";
  }
  const remote = named
    .split(",")
    .map((host) => host.trim())
    .filter((host) => host !== "" && !host.startsWith("/"))
    .filter((host) => !loopbackHosts.includes(host.toLowerCase()));
  if (remote.length === 0) return undefined;
  return (
    "DATABASE_URL must name a loopback Postgres (127.0.0.1, localhost or [::1]) or a Unix " +
    `socket, not ${remote.join(", ")}: this command truncates it`
  );
}

/** `value` as one shell word, safe to paste whatever it holds. The same rule as `quote` in
 *  packages/envoy/scripts/dev-postgres.sh; both tests hold it to
 *  packages/envoy/scripts/fixtures/shell-quote.json. */
export function shellQuote(value: string): string {
  return /^[A-Za-z0-9_@%+=:,./-]+$/.test(value) ? value : `'${value.replaceAll("'", `'\\''`)}'`;
}

/** How long the live view's answer may take: Dispatch answers a subscription it accepts with the
 *  responder verdict as soon as it has one, and refuses one it does not at once. */
const AGENT_VIEW_WAIT_MS = 10_000;

const BunWebSocket = WebSocket as unknown as new (
  url: string,
  options: { readonly headers: Record<string, string> }
) => WebSocket;

/**
 * Why a seeded session's live view (`/agents/<id>/live`) would read "This session's conversation
 * is unavailable", or undefined when Dispatch carries it: it subscribes the agent view on the live
 * socket as the page does, under `headers` (a signed-in person's cookie), and reads Dispatch's
 * first frame for it. A session id that is not a uuid is refused there (`SESSION_ID_INPUT`), which
 * is how a seed of named sessions passes every HTTP check and still has no live view.
 */
export async function agentViewRefusal(
  baseUrl: string,
  headers: Record<string, string>,
  sessionId: string
): Promise<string | undefined> {
  const socketUrl = new URL("/ws/live", baseUrl);
  socketUrl.protocol = socketUrl.protocol === "https:" ? "wss:" : "ws:";
  const sub = "local-instance-smoke";
  const answered = Promise.withResolvers<string | undefined>();
  const socket = new BunWebSocket(socketUrl.href, { headers });
  const settle = (reason: string | undefined) => {
    answered.resolve(reason);
    socket.close(1000);
  };
  const timeout = setTimeout(
    () => settle(`Dispatch sent nothing for it within ${AGENT_VIEW_WAIT_MS / 1_000}s`),
    AGENT_VIEW_WAIT_MS
  );
  socket.addEventListener("open", () => {
    socket.send(
      JSON.stringify({ sub, type: "subscribe", view: { kind: "agent", session_id: sessionId } })
    );
  });
  socket.addEventListener("message", (message) => {
    let frame: { readonly type?: string; readonly sub?: string; readonly code?: string };
    try {
      frame = JSON.parse(String(message.data));
    } catch {
      return;
    }
    if (frame.sub !== sub) return;
    if (frame.type === "agent") settle(undefined);
    else settle(`Dispatch answered ${JSON.stringify(frame)}`);
  });
  socket.addEventListener("close", (event) =>
    settle(`the live socket closed first (${event.code} ${event.reason})`)
  );
  socket.addEventListener("error", () => settle("the live socket could not be opened"));
  try {
    return await answered.promise;
  } finally {
    clearTimeout(timeout);
  }
}

/** One row the seed guarantees the unfiltered Inbox read returns. An ask of the seed's own is
 *  matched by its ask id; the spec's decision row is matched by its block id on its issue,
 *  because the document service may retract that ask and write it again for the same block.
 *  `made` is what the seed made the row, as a failure names it: `the blocking ask on CORE-5`. */
export type SeededInboxRow =
  | { readonly kind: "ask"; readonly askId: string; readonly made: string }
  | {
      readonly kind: "block";
      readonly blockId: string;
      readonly issueKey: string;
      readonly made: string;
    };

/** What the helper reads of an Inbox row; `getInbox`'s own rows satisfy it. */
export type InboxRowRead = Pick<InboxRow, "id" | "issue_key" | "block_id" | "document">;

export interface SeededInboxOptions {
  /** 15_000 by default: the deadline `waitForSpecAsk` already gives this settlement. */
  readonly deadlineMs?: number;
  /** 100 by default, the interval `waitForSpecAsk` uses. */
  readonly intervalMs?: number;
  readonly signal?: AbortSignal;
}

export interface SeededInboxMatch {
  readonly reads: number;
  /** `inbox rows: 5 of 5 declared rows matched in 1 read`. */
  readonly line: string;
}

/** The last line of every failure block: whose rows these are. */
const SEEDED_INBOX_OWNER =
  "  These rows are written by the seed (packages/dispatch/e2e/workspace.ts) and by Dispatch's " +
  "own document settlement, not by the change under test.";

/** What one Inbox read holds against the rows the seed declares. */
interface SeededInboxDiff {
  readonly missing: readonly SeededInboxRow[];
  readonly unexpected: readonly InboxRowRead[];
  /** How many rows the read listed. */
  readonly rowCount: number;
}

/** The difference between the rows `declared` and one Inbox read: each declared row, in order,
 *  takes the first Inbox row not yet taken that it names, so a second copy of a row is left over. */
function diffSeededInbox(
  declared: readonly SeededInboxRow[],
  rows: readonly InboxRowRead[]
): SeededInboxDiff {
  const taken = new Set<number>();
  const missing: SeededInboxRow[] = [];
  for (const want of declared) {
    const index = rows.findIndex(
      (row, at) =>
        !taken.has(at) &&
        (want.kind === "ask"
          ? row.id === want.askId
          : row.block_id === want.blockId && row.issue_key === want.issueKey)
    );
    if (index === -1) missing.push(want);
    else taken.add(index);
  }
  const unexpected = rows.filter((_, at) => !taken.has(at));
  return { missing, rowCount: rows.length, unexpected };
}

const plural = (count: number, word: string) => `${count} ${word}${count === 1 ? "" : "s"}`;

/** `items` as the block's list lines, or one `none` line for an empty list. */
const listed = (items: readonly string[]) =>
  (items.length === 0 ? ["none"] : items).map((item) => `    - ${item}`);

/** `step`, or an Error once `ms` pass with it still unsettled, so one read that never answers
 *  cannot hold the wait forever. `step`'s own outcome is always handled, and the timer is cleared
 *  as soon as it settles. */
function answeredWithin<T>(step: Promise<T>, ms: number): Promise<T> {
  const { promise, resolve, reject } = Promise.withResolvers<T>();
  const timer = setTimeout(() => {
    reject(new Error(`the read had not answered within ${(ms / 1_000).toFixed(1)} s`));
  }, ms);
  step.then(resolve, reject).finally(() => clearTimeout(timer));
  return promise;
}

/**
 * Reads the Inbox with `read` until it lists exactly the rows `declared`, and answers the pass
 * line, or throws a block that names every missing and every unexpected row once `deadlineMs`
 * has passed. `read` is ONE UNFILTERED Inbox read (`GET /api/v1/inbox` with no query): the seed
 * declares that set, and a filtered read (`?assignee=unassigned`, say) holds another.
 *
 * Each declared row takes the first Inbox row it names that no declared row took before it; a
 * declared row that takes none is missing, and an Inbox row that none took is unexpected, a
 * second copy of a declared row included. A read that throws counts as a read and is tried again
 * to the same deadline. Each read may take the time the deadline leaves when it starts, or one
 * interval where less is left, and one that takes longer is a failed read; no read starts once the
 * deadline has passed, so the wait ends within about the deadline and one interval whatever the
 * server does. The abort of `signal` rejects with Interrupted at once, whether the helper waits on
 * a read or sleeps between reads.
 */
export async function awaitSeededInbox(
  declared: readonly SeededInboxRow[],
  read: () => Promise<readonly InboxRowRead[]>,
  options: SeededInboxOptions = {}
): Promise<SeededInboxMatch> {
  const { deadlineMs = 15_000, intervalMs = 100, signal } = options;
  if (signal?.aborted) throw new Interrupted("interrupted");
  const started = Date.now();
  let reads = 0;
  let answered: SeededInboxDiff | undefined;
  let failure: { readonly error: unknown } | undefined;
  for (;;) {
    reads += 1;
    try {
      // The time the deadline leaves this read, or one interval where less is left, so a read
      // that hangs near the deadline holds the wait one interval past it at most.
      const budget = Math.max(deadlineMs - (Date.now() - started), intervalMs);
      const step = answeredWithin((async () => read())(), budget);
      answered = diffSeededInbox(declared, await untilAborted(step, signal));
      failure = undefined;
      if (answered.missing.length === 0 && answered.unexpected.length === 0) {
        return {
          line: `inbox rows: ${declared.length} of ${declared.length} declared rows matched in ${plural(reads, "read")}`,
          reads,
        };
      }
    } catch (error) {
      if (signal?.aborted) throw new Interrupted("interrupted");
      failure = { error };
    }
    const left = deadlineMs - (Date.now() - started);
    if (left > 0) {
      // The interval between two Inbox reads while the document settlement lands, cut to the
      // deadline, so the wait ends at the deadline rather than one interval past it.
      await untilAborted(sleep(Math.min(intervalMs, left), undefined, { signal }), signal);
    }
    const elapsed = Date.now() - started;
    if (elapsed >= deadlineMs) {
      throw new Error(seededInboxFailure(declared, elapsed, reads, answered, failure));
    }
  }
}

/** The failure block of `awaitSeededInbox`: the newest difference it read, and the error of the
 *  last read where that read threw. */
function seededInboxFailure(
  declared: readonly SeededInboxRow[],
  elapsedMs: number,
  reads: number,
  answered: SeededInboxDiff | undefined,
  failure: { readonly error: unknown } | undefined
): string {
  const seconds = (elapsedMs / 1_000).toFixed(1);
  const lines =
    answered === undefined
      ? [
          `inbox rows: no read of the Inbox answered in ${seconds} s over ${plural(reads, "read")}.`,
          `  the seed declares ${plural(declared.length, "row")}.`,
        ]
      : [
          `inbox rows: the Inbox and the seed still disagree after ${seconds} s and ${plural(reads, "read")}.`,
          "  missing (the seed declares it, the Inbox did not list it):",
          ...listed(
            answered.missing.map((row) =>
              row.kind === "ask"
                ? `${row.made} (ask ${row.askId})`
                : `${row.made} (block ${row.blockId})`
            )
          ),
          "  unexpected (the Inbox listed it, the seed declares no such row):",
          ...listed(
            answered.unexpected.map((row) => {
              const on =
                row.issue_key ??
                (row.document === undefined ? "no issue" : `the document ${row.document.name}`);
              return `ask ${row.id} on ${on} (block ${row.block_id ?? "none"})`;
            })
          ),
          `  the Inbox answered ${plural(answered.rowCount, "row")}; the seed declares ${declared.length}.`,
        ];
  if (failure !== undefined) {
    const { error } = failure;
    lines.push(`  the last read failed: ${error instanceof Error ? error.message : String(error)}`);
  }
  lines.push(SEEDED_INBOX_OWNER);
  return lines.join("\n");
}
