/** What `e2e/local-instance.ts` and `e2e/harness-start.ts` run on: the processes they start and
 *  stop, the refusal of a database the command must not truncate, and the quoting of the commands
 *  it prints; and the waits the command and the seed (`e2e/workspace.ts`) make for the fact they
 *  read next, each until a bound its caller sets: PostgreSQL's answer (`awaitPostgres`), the
 *  spec's decision block (`awaitDecisionBlock`) and the Inbox rows the seed declares
 *  (`awaitSeededInbox`), each a read repeated until a deadline (`awaitFact`), and the live view's
 *  first frame (`agentViewRefusal`). Nothing here runs on import, and nothing here imports a value
 *  of another e2e module, so `e2e/__tests__/local-instance-support.test.ts` drives it without a
 *  server. */
import { type ChildProcess, spawn } from "node:child_process";
import type { Readable } from "node:stream";
import { setTimeout as sleep } from "node:timers/promises";
import type { Ask, InboxRow } from "../web/src/api/types";

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

/** The reaper's program (bash, whose `kill` takes a negative group id after `--`, where dash's does
 *  not; every harness script is bash already). It reads one line per thing to end from its stdin,
 *  the write end of which only the process that started it holds:
 *
 *  - `group <pgid>`: a process group, ended as `Supervisor.stop` ends one: SIGTERM, then SIGKILL to
 *    whatever still runs ten seconds later;
 *  - `container <name>`: a container, removed with its volumes by the name its starter gave it
 *    before it ran `docker run`, which is unique to the run;
 *  - `cluster <pg_ctl>` TAB `<data directory>`: a Postgres cluster, stopped `immediate`;
 *  - `forget <line>`: one of those its starter already ended itself;
 *  - `end`: its starter stopped everything, and it exits.
 *
 *  End of input with no `end` is that process gone without a stop - killed, crashed, out of
 *  memory - and it ends everything it holds. Each line names exactly one thing its starter made,
 *  by the name it recorded before making it, never a pattern. A start the kill interrupted - a
 *  `docker run --name <name>`, a `pg_ctl -D <data> start` - outlives its starter and can make its
 *  container or its server after the reaper's first removal, so the reaper removes again while a
 *  process naming that container or that data directory still runs, for at most a minute. */
const reaperProgram = `held=
while IFS= read -r line; do
  case "$line" in
    end) exit 0 ;;
    "forget "*)
      target=\${line#forget }
      kept=
      while IFS= read -r item; do
        [ -n "$item" ] && [ "$item" != "$target" ] && kept+="$item"$'\\n'
      done <<< "$held"
      held=$kept ;;
    *) held+="$line"$'\\n' ;;
  esac
done
end_container() {
  local tries=0
  while [ "$tries" -lt 120 ]; do
    docker rm --force --volumes "$1" >/dev/null 2>&1
    if ! pgrep -f -- "--name $1( |$)" >/dev/null 2>&1 &&
      ! docker container inspect "$1" >/dev/null 2>&1; then
      return
    fi
    sleep 0.5
    tries=$((tries + 1))
  done
}
end_cluster() {
  local tries=0
  while [ "$tries" -lt 120 ]; do
    "$1" -D "$2" -m immediate -w stop >/dev/null 2>&1
    pgrep -f -- "-D \${2//./\\\\.}( |$)" >/dev/null 2>&1 || return
    sleep 0.5
    tries=$((tries + 1))
  done
}
groups=()
while IFS= read -r item; do
  case "$item" in
    "group "*)
      groups+=("\${item#group }")
      kill -TERM -- "-\${item#group }" 2>/dev/null ;;
    "container "*)
      end_container "\${item#container }" & ;;
    "cluster "*)
      rest=\${item#cluster }
      end_cluster "\${rest%%$'\\t'*}" "\${rest#*$'\\t'}" & ;;
  esac
done <<< "$held"
waited=0
while [ "\${#groups[@]}" -gt 0 ] && [ "$waited" -lt 100 ]; do
  alive=
  for group in "\${groups[@]}"; do kill -0 -- "-$group" 2>/dev/null && alive=1; done
  [ -z "$alive" ] && break
  sleep 0.1
  waited=$((waited + 1))
done
for group in "\${groups[@]}"; do kill -KILL -- "-$group" 2>/dev/null; done
wait`;

/** One thing a reaper ends when its starter dies without ending it: a process group, a container
 *  by the name its starter gave it, or a Postgres cluster by its `pg_ctl` and its data directory.
 *  A container and a cluster are handed over before they are started, so a starter killed while
 *  one starts leaves nothing the reaper does not know of. */
export type Reaped =
  | { readonly group: number }
  | { readonly container: string }
  | { readonly cluster: { readonly pgCtl: string; readonly data: string } };

/** The line `reaperProgram` reads for `reaped`. */
function reaperLine(reaped: Reaped): string {
  if ("group" in reaped) return `group ${reaped.group}`;
  if ("container" in reaped) return `container ${reaped.container}`;
  return `cluster ${reaped.cluster.pgCtl}\t${reaped.cluster.data}`;
}

/** What ends the processes, containers and clusters this process starts when this process dies
 *  without ending them: a SIGKILL, an out-of-memory kill, a crashed test runner. A stop is
 *  something this process does, so its death runs none. The first `watch` starts a reaper
 *  (`reaperProgram`) in a session of its own, so no signal meant for this process reaches it, and
 *  tells it each thing as it is made; its stdin is a pipe only this process writes, so this
 *  process's death, however it comes, is the reaper's end of input, and the reaper ends what it
 *  holds. The reaper is unreferenced, its pipe too, so neither keeps this process alive; a reaper
 *  already gone answers a write with EPIPE, which is ignored, since it can end nothing either way. */
export class Reaper {
  #process: ChildProcess | undefined;

  /** Hands `reaped` to the reaper, which ends it should this process die first. */
  watch(reaped: Reaped): void {
    if (this.#process === undefined) {
      this.#process = spawn("bash", ["-c", reaperProgram], {
        detached: true,
        stdio: ["pipe", "ignore", "ignore"],
      });
      this.#process.on("error", () => {});
      this.#process.stdin?.on("error", () => {});
      this.#process.unref();
      (this.#process.stdin as { unref?: () => void } | null)?.unref?.();
    }
    this.#process.stdin?.write(`${reaperLine(reaped)}\n`);
  }

  /** Takes back `reaped`, which this process has ended itself: the reaper never touches an id the
   *  system may since have handed to something else. */
  forget(reaped: Reaped): void {
    this.#process?.stdin?.write(`forget ${reaperLine(reaped)}\n`);
  }

  /** Everything handed to the reaper is ended: it exits, ending nothing. */
  end(): void {
    this.#process?.stdin?.end("end\n");
    this.#process = undefined;
  }
}

/** Every process the command starts, as the leader of a process group of its own, and the one
 *  signal that a stop is under way: a Ctrl-C (`interrupt`) or the end of the run (`stop`). Each
 *  group is handed to a `Reaper` as it starts, so a process that dies without a stop leaves none
 *  of them running. */
export class Supervisor {
  readonly #children: Running[] = [];
  readonly #stopping = new AbortController();
  readonly #reaper = new Reaper();
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
    if (child.pid !== undefined) this.#reaper.watch({ group: child.pid });
    const running = { child, exited: ended.promise, name };
    this.#children.push(running);
    return running;
  }

  /** A foreground step. It throws on a non-zero exit, so a failed web build or dev-postgres.sh
   *  reaches the command's one catch (and `stop`) like every other failure. A step that has ended
   *  is given back to the reaper, as `stop` gives back every group it ends: its group's id is free
   *  for the system to give to another process from then on. */
  async run(argv: readonly string[], cwd: string): Promise<void> {
    const step = this.start(argv.join(" "), argv, cwd);
    const code = await this.untilInterrupted(step.exited);
    this.#children.splice(this.#children.indexOf(step), 1);
    if (step.child.pid !== undefined) this.#reaper.forget({ group: step.child.pid });
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
   *  prompt waiting on it. Answers whether the force-kill ran. */
  async stop(forceAfterMs = 10_000): Promise<boolean> {
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
    let forced = false;
    const forceKill = setTimeout(() => {
      forced = true;
      signalGroups("SIGKILL");
    }, forceAfterMs);
    await Promise.allSettled(running.map(({ exited }) => exited));
    clearTimeout(forceKill);
    // Every group this stop signalled is gone, so the reaper has nothing left to end: `end` lets
    // it go without signalling a group number the system may since have handed to another
    // process.
    this.#reaper.end();
    return forced;
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

const plural = (count: number, word: string) => `${count} ${word}${count === 1 ? "" : "s"}`;

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

/** One wait of `awaitFact`: the fact its caller reads next, and how to read it. */
interface FactWait<T> {
  /** The fact as a sentence, which a failure starts with: `PostgreSQL at 127.0.0.1:55432
   *  answers \`select 1\``. */
  readonly fact: string;
  /** One read. `leftMs` is the time it may take: what the deadline leaves it, or one interval
   *  where less is left. A read still unsettled after that is a failed read. */
  readonly read: (leftMs: number) => Promise<T>;
  /** Whether a value read is the fact. */
  readonly holds: (value: T) => boolean;
  /** A value read, as a failure names it. */
  readonly show: (value: T) => string;
  /** The lines a failure ends with, after what the reads answered: what a reader of the failure
   *  needs beside the fact. None where the wait gives none. */
  readonly closing?: readonly string[];
  /** How long the wait may take, in milliseconds. Its caller sets it: no wait here holds a bound
   *  of its own, so a caller's wait gives up at the bound that caller's own checks use. */
  readonly boundMs: number;
  /** The pause between two reads. */
  readonly intervalMs: number;
  readonly signal?: AbortSignal;
}

/** What `awaitFact` answers: the value the fact held for, and how many reads it took. */
interface FactHeld<T> {
  readonly value: T;
  readonly reads: number;
}

/** The parts of a wait that gave up, which `FactGaveUp` names. */
interface GaveUp {
  readonly fact: string;
  readonly elapsedMs: number;
  readonly reads: number;
  /** The last value a read answered, shown; undefined when no read answered. */
  readonly lastShown: string | undefined;
  /** The error of the last read, where that read failed. */
  readonly lastFailure: string | undefined;
  readonly closing: readonly string[];
}

/** What `awaitFact` throws once its deadline has passed with the fact not holding: the fact, the
 *  time waited, the reads, the last value read, the last read's error where it failed, and the
 *  wait's closing lines, which it writes as the whole message. */
class FactGaveUp extends Error {
  constructor({ closing, elapsedMs, fact, lastFailure, lastShown, reads }: GaveUp) {
    const seconds = (elapsedMs / 1_000).toFixed(1);
    const lines =
      lastShown === undefined
        ? [`${fact}: no read answered in ${seconds} s over ${plural(reads, "read")}.`]
        : [
            `${fact}: still not so after ${seconds} s and ${plural(reads, "read")}.`,
            `  the last read answered: ${lastShown}`,
          ];
    if (lastFailure !== undefined) lines.push(`  the last read failed: ${lastFailure}`);
    super([...lines, ...closing].join("\n"));
  }
}

/**
 * Reads with `read` until `holds` answers true for the value read, and answers that value, or
 * throws FactGaveUp once the deadline has passed. The deadline is `boundMs` from the moment the
 * wait starts. A read that throws counts as a read and is tried again to the same deadline. Each
 * read may take the time the deadline leaves when it starts, or one interval where less is left,
 * and one that takes longer is a failed read; the sleep before the deadline is cut to the time
 * left, and no read starts once the deadline has passed, so the wait ends within one interval of
 * the deadline whatever the reads do. The abort of `signal` rejects with Interrupted at once,
 * whether the wait is on a read or between reads.
 */
async function awaitFact<T>(wait: FactWait<T>): Promise<FactHeld<T>> {
  const { boundMs, closing = [], fact, holds, intervalMs, read, show, signal } = wait;
  if (signal?.aborted) throw new Interrupted("interrupted");
  const started = Date.now();
  const deadline = started + boundMs;
  let reads = 0;
  let last: { readonly value: T } | undefined;
  let failure: string | undefined;
  for (;;) {
    reads += 1;
    try {
      // The time the deadline leaves this read, or one interval where less is left, so a read
      // that hangs near the deadline holds the wait one interval past it at most.
      const budget = Math.max(deadline - Date.now(), intervalMs);
      const value = await untilAborted(
        answeredWithin((async () => read(budget))(), budget),
        signal
      );
      last = { value };
      failure = undefined;
      if (holds(value)) return { reads, value };
    } catch (error) {
      if (signal?.aborted) throw new Interrupted("interrupted");
      failure = error instanceof Error ? error.message : String(error);
    }
    const left = deadline - Date.now();
    if (left > 0) {
      // The interval between two reads while the fact is on its way, cut to the deadline, so
      // the wait ends at the deadline rather than one interval past it.
      await untilAborted(sleep(Math.min(intervalMs, left), undefined, { signal }), signal);
    }
    if (Date.now() >= deadline) {
      throw new FactGaveUp({
        closing,
        elapsedMs: Date.now() - started,
        fact,
        lastFailure: failure,
        lastShown: last === undefined ? undefined : show(last.value),
        reads,
      });
    }
  }
}

/** One attempt of `awaitPostgres`: runs `query` and answers what psql printed, or fails with
 *  psql's error. It ends its psql once `limitMs` have passed, so an attempt against a server
 *  that takes the connection and never answers ends within the time the wait left it. */
export type PostgresProbe = (query: string, limitMs: number) => Promise<string>;

/** Runs `probe` until PostgreSQL at `address` answers `select 1`, as a server that has just
 *  started does once it takes connections, and throws naming the address, the query and psql's
 *  last error once `boundMs` has passed. `address` is what a failure names; the probe holds the
 *  connection string. */
export async function awaitPostgres(
  address: string,
  probe: PostgresProbe,
  boundMs: number,
  signal?: AbortSignal
): Promise<void> {
  const query = "select 1";
  await awaitFact({
    boundMs,
    fact: `PostgreSQL at ${address} answers \`${query}\``,
    holds: (printed) => printed === "1",
    // Half a second between attempts while the container starts, so the wait sees the server
    // within about half a second of its taking connections.
    intervalMs: 500,
    read: (leftMs) => probe(query, leftMs),
    show: (printed) => JSON.stringify(printed),
    signal,
  });
}

const BunWebSocket = WebSocket as unknown as new (
  url: string,
  options: { readonly headers: Record<string, string> }
) => WebSocket;

/**
 * Why a seeded session's live view (`/agents/<id>/live`) would read "This session's conversation
 * is unavailable", or undefined when Dispatch carries it: it subscribes the agent view on the live
 * socket as the page does, under `headers` (a signed-in person's cookie), and reads Dispatch's
 * first frame for it. A session id that is not a uuid is refused there (`SESSION_ID_INPUT`), which
 * is how a seed of named sessions passes every HTTP check and still has no live view. `boundMs` is
 * how long that first frame may take before its absence is the refusal, which then names what the
 * socket did receive: the frames of other subscriptions, or nothing. It reads no more than once:
 * one subscription, one timer.
 */
export async function agentViewRefusal(
  baseUrl: string,
  headers: Record<string, string>,
  sessionId: string,
  boundMs: number
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
  // The frames that were not Dispatch's answer to this subscription, which a timeout names.
  let others = 0;
  let lastOther = "";
  const timeout = setTimeout(() => {
    const received =
      others === 0
        ? "the socket received nothing"
        : `the socket received ${plural(others, "other frame")}, the last ${lastOther}`;
    settle(
      `Dispatch's first frame for the agent view of ${sessionId} (subscription ${sub}) had not ` +
        `come after ${(boundMs / 1_000).toFixed(1)} s; ${received}`
    );
  }, boundMs);
  socket.addEventListener("open", () => {
    socket.send(
      JSON.stringify({ sub, type: "subscribe", view: { kind: "agent", session_id: sessionId } })
    );
  });
  socket.addEventListener("message", (message) => {
    const text = String(message.data);
    let frame: { readonly type?: string; readonly sub?: string; readonly code?: string } | null;
    try {
      frame = JSON.parse(text);
    } catch {
      frame = null;
    }
    if (frame?.sub !== sub) {
      others += 1;
      lastOther = text;
      return;
    }
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

/** What the decision-block wait reads of an issue's open ask; the issue detail's `open_asks`
 *  satisfy it. */
export type OpenAskRead = Pick<Ask, "id" | "block_id">;

/**
 * The block id of the decision `issueKey`'s spec declares, once one read of the issue's open asks
 * (`read`) names it. The decision block becomes an ask only when the document service settles the
 * spec, which happens after `createIssue` answers, and the service may retract that ask and write
 * it again for the same block, so the block id, not the ask id, names the decision's Inbox row.
 * Open asks that name more than one block fail at once, since the spec declares one. A read that
 * names none is read again until `boundMs` has passed, and the failure names the open asks the
 * last read listed.
 */
export async function awaitDecisionBlock(
  issueKey: string,
  read: () => Promise<readonly OpenAskRead[]>,
  boundMs: number
): Promise<string> {
  const {
    value: { blockIds },
  } = await awaitFact({
    boundMs,
    fact: `seedWorkspace: ${issueKey}'s open asks name its spec's decision block`,
    holds: (found) => found.blockIds.length > 0,
    // A tenth of a second between reads, so the seed goes on within about a tenth of a second of
    // the block's ask being written.
    intervalMs: 100,
    read: async () => {
      const asks = await read();
      const blockIds = [...new Set(asks.flatMap((ask) => (ask.block_id ? [ask.block_id] : [])))];
      return { asks, blockIds };
    },
    show: (found) =>
      found.asks.length === 0
        ? "no open ask"
        : found.asks.map((ask) => `ask ${ask.id} (block ${ask.block_id ?? "none"})`).join(", "),
  });
  const [blockId, ...more] = blockIds;
  if (more.length > 0) {
    throw new Error(
      `seedWorkspace: ${issueKey}'s open asks name more than one block: ${blockIds.join(", ")}`
    );
  }
  return blockId;
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
  /** How long the wait may take, in milliseconds: its caller's bound, with no default. */
  readonly boundMs: number;
  /** 100 by default, the interval `awaitDecisionBlock` reads at. */
  readonly intervalMs?: number;
  readonly signal?: AbortSignal;
}

export interface SeededInboxMatch {
  readonly reads: number;
  /** `inbox rows: 5 of 5 declared rows matched in 1 read`. */
  readonly line: string;
}

/** The last line of every failure of `awaitSeededInbox`: whose rows these are. */
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

/** `items` as the block's list lines, or one `none` line for an empty list. */
const listed = (items: readonly string[]) =>
  (items.length === 0 ? ["none"] : items).map((item) => `    - ${item}`);

/** One Inbox read as its failure shows it after "the last read answered": how many rows it
 *  listed, then every missing and every unexpected row. */
function shownInboxRead(diff: SeededInboxDiff): string {
  return [
    plural(diff.rowCount, "row"),
    "  missing (the seed declares it, the Inbox did not list it):",
    ...listed(
      diff.missing.map((row) =>
        row.kind === "ask" ? `${row.made} (ask ${row.askId})` : `${row.made} (block ${row.blockId})`
      )
    ),
    "  unexpected (the Inbox listed it, the seed declares no such row):",
    ...listed(
      diff.unexpected.map((row) => {
        const on =
          row.issue_key ??
          (row.document === undefined ? "no issue" : `the document ${row.document.name}`);
        return `ask ${row.id} on ${on} (block ${row.block_id ?? "none"})`;
      })
    ),
  ].join("\n");
}

/**
 * Reads the Inbox with `read` until it lists exactly the rows `declared`, and answers the pass
 * line, or throws a block that names every missing and every unexpected row once `boundMs` has
 * passed. `read` is ONE UNFILTERED Inbox read (`GET /api/v1/inbox` with no query): the seed
 * declares that set, and a filtered read (`?assignee=unassigned`, say) holds another.
 *
 * Each declared row takes the first Inbox row it names that no declared row took before it; a
 * declared row that takes none is missing, and an Inbox row that none took is unexpected, a
 * second copy of a declared row included. The reads, their deadline and the abort of `signal`
 * follow `awaitFact`, so the wait ends within about `boundMs` and one interval whatever the
 * server does.
 */
export async function awaitSeededInbox(
  declared: readonly SeededInboxRow[],
  read: () => Promise<readonly InboxRowRead[]>,
  options: SeededInboxOptions
): Promise<SeededInboxMatch> {
  const { boundMs, intervalMs = 100, signal } = options;
  const { reads } = await awaitFact({
    boundMs,
    closing: [`  the seed declares ${plural(declared.length, "row")}.`, SEEDED_INBOX_OWNER],
    fact:
      "inbox rows: the Inbox lists exactly the " +
      `${plural(declared.length, "row")} the seed declares`,
    holds: (diff) => diff.missing.length === 0 && diff.unexpected.length === 0,
    intervalMs,
    read: async () => diffSeededInbox(declared, await read()),
    show: shownInboxRead,
    signal,
  });
  return {
    line: `inbox rows: ${declared.length} of ${declared.length} declared rows matched in ${plural(reads, "read")}`,
    reads,
  };
}
