/** The children of the rows of `harness-postgres-leftover.test.ts`, and the starts of a stack in a
 *  row's own process: the stacks, the starts of the pool and the shell stacks those children run,
 *  their stops, and what each of them waits for. Docker's reads, the removals of what a row made,
 *  and the marks, directories and containers that a row plants are in
 *  `harness-postgres-leftover-fixtures.ts`. `bun test` collects only `*.test.ts` and its kin, so
 *  this module runs nowhere of its own. It holds no assertion: each check of a row is in the test
 *  file.
 *
 *  A child is a process that `launch` starts, records by its pid and its start time, and collects
 *  the output of. A SIGKILL goes only to such a child, while `/proc` still names that process.
 *
 *  A CHILD'S LIMIT IS THE SUM OF WHAT IT WAITS FOR. Each child a row waits for here, a start of the
 *  pool, a stack's start or its stop, a shell's start, is a part of the row's waits (`Part` of
 *  `harness-postgres-waits.ts`), whose bound the program sums from the bounds of the waits inside
 *  the child, the start's own included, and the fixture that waits for it charges the row first
 *  (`Row.part`). So no child is killed inside its own bounds, and a row whose limit does not count
 *  a part fails there, naming it. */

import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { setTimeout as sleep } from "node:timers/promises";
import { fileURLToPath } from "node:url";
import type { Subprocess } from "bun";
import { harnessListeners } from "../harness-listeners";
import {
  pgBinVariable,
  pgConnectionsVariable,
  pgImageVariable,
  readyMs,
  type StackDatabase,
  type StackPostgres,
} from "../harness-postgres";
import { dockerMs, removalBounds } from "../harness-postgres-container-removal";
import { runDirectoryPrefix, startOf } from "../harness-postgres-marks";
import { claimWaitMs } from "../harness-postgres-removal";
import {
  boundOf,
  claimEventMs,
  during,
  inTurn,
  type Part,
  partOf,
  type Row,
  type Waits,
} from "./harness-postgres-waits";
import type { ProviderVariable } from "./postgres-provider";

const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));
const harnessPostgres = fileURLToPath(new URL("../harness-postgres.ts", import.meta.url));
const harnessPool = fileURLToPath(new URL("../harness-pool.ts", import.meta.url));

/** A stack that starts its Postgres as the pool starts each stack's, prints where it serves,
 *  and stops it once its stdin closes. A SIGKILL stops nothing. */
const stackScript = `
import { mkdtempSync, readlinkSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { stackPostgres } from ${JSON.stringify(harnessPostgres)};
const dir = mkdtempSync(join(tmpdir(), ${JSON.stringify(runDirectoryPrefix)}));
const database = await stackPostgres({ dir, run: process.env.LEFTOVER_RUN }).start(0);
console.log(JSON.stringify({ dir, url: database.url, address: database.address, pidNamespace: readlinkSync("/proc/self/ns/pid") }));
for await (const _ of Bun.stdin.stream()) {}
await database.stop(false);
rmSync(dir, { recursive: true, force: true });
`;

/** The pool's start, given no provider: it removes what stacks that are gone left, and then
 *  refuses two stacks without a Postgres, which is the line it prints. */
const startScript = `
import { startStacks } from ${JSON.stringify(harnessPool)};
try {
  await startStacks(2, { env: {} });
  console.log("started");
} catch (error) {
  console.log("refused: " + error.message);
}
`;

/** What the refusal of a start says: the pool's start ran up to its provider check. */
export const refused =
  "refused: 2 Playwright workers run 2 stacks, each with a Postgres of its own";

/** A whole start of the pool, `startStacks`, with the provider of its environment: it removes what
 *  stacks that are gone left, starts one stack's Postgres and the harness a deployed run starts
 *  (the fake Envoy and the plain-HTTP proxy, which `PLAYWRIGHT_BASE_URL` selects, so no Go build
 *  runs), prints the stack's database, and stops it all once its stdin closes. */
const poolScript = `
import { startStacks, stopStacks } from ${JSON.stringify(harnessPool)};
const stacks = await startStacks(1, { env: process.env });
console.log(JSON.stringify({ url: stacks[0]?.env.DATABASE_URL }));
for await (const _ of Bun.stdin.stream()) {}
await stopStacks(false);
`;

/** How long `pg_ctl -w` waits for a server to start or to stop before it gives up: `PGCTLTIMEOUT`,
 *  60 s by default, which the binaries provider leaves unset (e2e/harness-postgres.ts). */
export const pgCtlMs = 60_000;

/** What a `bun` child waits for before its own work: its start, which may take `claimEventMs`
 *  under load (`Waits.children`). */
const bunStart: Waits = { children: 1, reads: 0, bounds: [] };

/** What a start of one stack's Postgres waits for, one wait after another (`start` of
 *  `stackPostgres` of e2e/harness-postgres.ts), for the provider `variable` names. The image
 *  provider's: its `docker create` and its `docker start`, each a call of the daemon that makes or
 *  starts a container, which the provider bounds by nothing, at the fixtures' time for such a call
 *  (`makeMs`), and then the image's initialisation and its server's first answer, which it gives
 *  `readyMs` together. The binaries provider's: the template's `pg_ctl -w start` and `stop` and
 *  the stack's `pg_ctl -w start`, each at `pgCtlMs`, and the template's and the stack's first
 *  answer, each at `readyMs`. Its `initdb`, its `cp -a` of the template, the `psql`s of both and
 *  the image provider's reads of the container's log it bounds by nothing either, and no reading
 *  of them under load is held: they take the room above the part (`partOf`). */
function providerStart(variable: ProviderVariable): Waits {
  return variable === pgImageVariable
    ? { children: 0, reads: 0, makes: 2, bounds: [readyMs] }
    : { children: 0, reads: 0, bounds: [3 * pgCtlMs, 2 * readyMs] };
}

/** What a stop of one stack's Postgres waits for (its `stop`, e2e/harness-postgres.ts), for the
 *  provider `variable` names: the image provider's `docker rm` of its container, which it bounds
 *  by nothing, at the start's own bound for the removal of one container
 *  (`removalBounds.boundMs`), and the binaries provider's `pg_ctl -m immediate -w stop`, at
 *  `pgCtlMs`. */
function providerStop(variable: ProviderVariable): Waits {
  const ms = variable === pgImageVariable ? removalBounds.boundMs : pgCtlMs;
  return { children: 0, reads: 0, bounds: [ms] };
}

/** What a start's removal of what dead stacks left finds of a row's plants, which its waits are
 *  made of (`removalOf`): each container of a dead stack that it removes (`containers`), each mark
 *  of a dead stack in a run directory whose directories it claims (`marks`), and each part of
 *  those that another start claims at the same time, whose claim it waits for (`claims`). */
export interface Found {
  readonly containers?: number;
  readonly marks?: number;
  readonly claims?: number;
}

/** What a start's removal of what dead stacks left (`removeLeftoverPostgres` of
 *  e2e/harness-postgres-removal.ts) waits for, one wait after another, where it finds `found`:
 *  its `docker ps` of the marked containers, which it gives up at `dockerMs`; for each container,
 *  the removal, within `removalBounds.boundMs`, and the holder read before it removes the
 *  container's socket directory; for each mark, the holder read before its claim of each of the
 *  mark's two directories, and the stop of its postmaster, with the reads after it; and, for each
 *  part another start claims, its wait for that claim (`claimWaitMs`). What it finds of other runs
 *  on a Docker daemon or among the processes of a user that runs share is a wait no row counts:
 *  a CI job's daemon and runner are its own, and this lane's runs of the rows give each row a PID
 *  namespace and an owner label of its own. */
function removalOf({ containers = 0, marks = 0, claims = 0 }: Found): Waits {
  return {
    children: 0,
    reads: containers + 2 * marks,
    stops: marks,
    bounds: [dockerMs, containers * removalBounds.boundMs, claims * claimWaitMs],
  };
}

/** What the harness of a deployed run waits for as it starts (`startHarness` of
 *  e2e/harness-start.ts, with `PLAYWRIGHT_BASE_URL` set, as `poolScript` sets it): each listener
 *  a deployed run starts, one after another, each at its own bound (`bootMs` of
 *  e2e/harness-listeners.ts). */
const deployedHarness: Waits = {
  children: 0,
  reads: 0,
  bounds: harnessListeners.filter((listener) => listener.deployed).map(({ bootMs }) => bootMs),
};

/** A start of one stack in a child (`startStack`, `startNamespacedStack`): its `bun` start, and
 *  the provider's start of the stack (`providerStart`). */
export function stackStart(variable: ProviderVariable): Part {
  return partOf("a stack's start", inTurn(bunStart, providerStart(variable)));
}

/** A stop of one stack in a child (`stopStack`): the provider's stop (`providerStop`). */
export function stackStop(variable: ProviderVariable): Part {
  return partOf("a stack's stop", providerStop(variable));
}

/** A start of one stack in the row's own process (`startOwn`): the provider's start
 *  (`providerStart`). */
export function ownStart(variable: ProviderVariable): Part {
  return partOf("a stack's start in the row's own process", providerStart(variable));
}

/** A stop of one stack in the row's own process (`stopOwn`): the provider's stop
 *  (`providerStop`). */
export function ownStop(variable: ProviderVariable): Part {
  return partOf("a stack's stop in the row's own process", providerStop(variable));
}

/** A start of the pool that refuses, as `startScript` runs it (`startPool`): its `bun` start, and
 *  its removal of what dead stacks left, where it finds `found` (`removalOf`). */
export function poolStart(found: Found): Part {
  return partOf("a start of the pool", inTurn(bunStart, removalOf(found)));
}

/** A start of the pool that serves, as `poolScript` runs it (`startServingPool`): its `bun`
 *  start, its removal of what dead stacks left, where it finds `found` (`removalOf`), the
 *  provider's start of its one stack (`providerStart`), and its harness (`deployedHarness`). */
export function servingStart(variable: ProviderVariable, found: Found): Part {
  return partOf(
    "a start of the pool that serves",
    inTurn(bunStart, removalOf(found), providerStart(variable), deployedHarness)
  );
}

/** A stop of a start of the pool that serves (`stopPool`): its `stopStacks`, which stops the
 *  harness, signalling each process and killing what still runs 10 s later (`Supervisor.stop` of
 *  e2e/local-instance-support.ts), each end an event of another process (`claimEventMs`), and
 *  then the provider's stop of the stack (`providerStop`). */
export function poolStop(variable: ProviderVariable): Part {
  return partOf(
    "a stop of the pool",
    inTurn({ children: 0, reads: 0, bounds: [claimEventMs] }, providerStop(variable))
  );
}

/** A shell stack's start (`startShellStack`): its `postgres_mark`, a `bun` start, and its
 *  `docker run`, a call that makes a container (`makeMs`). */
export const shellStackStart = partOf("a shell stack's start", { ...bunStart, makes: 1 });

/** A shell's start of a stack as `scripts/e2e/lib/leftovers.sh` runs it (`shellSweep`): its
 *  `bun` start, and its removal of what dead stacks left, where it finds `found` (`removalOf`). */
export function shellSweepOf(found: Found): Part {
  return partOf("a shell's removal of what dead stacks left", inTurn(bunStart, removalOf(found)));
}

/** The end of a child that the row sent SIGKILL (`endOfKilled`): the kernel's, an event of
 *  another process (`claimEventMs`). */
export const killedEnd = partOf("the end of a child the row killed", {
  children: 0,
  reads: 0,
  bounds: [claimEventMs],
});

/** The start of the process `pid`, as `startOf` reads it from `/proc`, or "" where the process is
 *  gone. */
export function startTimeOf(pid: number): string {
  try {
    return startOf(readFileSync(`/proc/${pid}/stat`, "utf8"));
  } catch {
    return "";
  }
}

/** A child this file started, with what it printed so far. */
interface Child {
  readonly process: Subprocess<"pipe", "pipe", "pipe">;
  readonly pid: number;
  /** Its start, read as it spawned, which a signal checks before it is sent. */
  readonly start: string;
  readonly output: { stdout: string; stderr: string };
  /** Settles once both pipes have ended and `output` holds all they carried. */
  readonly drained: Promise<unknown>;
}

/** Reads `stream` to its end, handing each decoded chunk to `into`. */
async function collect(stream: ReadableStream<Uint8Array>, into: (text: string) => void) {
  const reader = stream.getReader();
  const decoder = new TextDecoder();
  for (;;) {
    const { done, value } = await reader.read();
    if (done) return;
    into(decoder.decode(value, { stream: true }));
  }
}

/** Starts `argv` with `env` in `dispatchPackage`, and collects what it prints. */
function launch(argv: readonly string[], env: Record<string, string | undefined>): Child {
  const child = Bun.spawn([...argv], {
    cwd: dispatchPackage,
    env,
    stdin: "pipe",
    stdout: "pipe",
    stderr: "pipe",
  });
  const output = { stdout: "", stderr: "" };
  const drained = Promise.all([
    collect(child.stdout, (text) => {
      output.stdout += text;
    }),
    collect(child.stderr, (text) => {
      output.stderr += text;
    }),
  ]);
  return { process: child, pid: child.pid, start: startTimeOf(child.pid), output, drained };
}

/** Starts `script` in a `bun` child with `env`. */
function spawn(script: string, env: Record<string, string | undefined>): Child {
  return launch(["bun", "--eval", script], env);
}

/** SIGKILL to `child`, a process this file started, only while `/proc` still names it: the pid
 *  with the start read as it spawned. */
export function kill(child: Child): void {
  if (child.start === "" || startTimeOf(child.pid) !== child.start) return;
  child.process.kill("SIGKILL");
}

/** A part of a row's waits that the row was charged for (`Row.part`): the row, and when the wait
 *  must end by where the row's CI step cut the row's limit, its deadline or, in its `finally`, the
 *  end of its stops. */
interface Charged {
  readonly row: Row;
  readonly by: number;
}

/** Charges `row` for `part` (`Row.part`), before the wait for it. */
export function charge(row: Row, part: Part): Charged {
  return { row, by: row.part(part) };
}

/** Waits for `child` to exit, and kills it and throws, naming `what`, where it has not within the
 *  bound of `part`, the part of its row's waits it is, which the caller charged its row for, or by
 *  the time that charge answered where the row's CI step cut its limit (`boundOf`). */
function exited(child: Child, part: Part, what: string, { row, by }: Charged): Promise<number> {
  return during(what, async () => {
    const { ms, named } = boundOf(row, part.ms, by);
    const code = await Promise.race([child.process.exited, sleep(ms).then(() => undefined)]);
    if (code !== undefined) return code;
    kill(child);
    throw new Error(`${what} did not end ${named}: ${child.output.stderr}`);
  });
}

/** Waits for the end of `child`, which the row `row` sent SIGKILL, as a part of the row's waits
 *  (`killedEnd`), and answers its exit status; throws, naming `what`, where it has not ended. */
export function endOfKilled(row: Row, child: Child, what: string): Promise<number> {
  return exited(child, killedEnd, what, charge(row, killedEnd));
}

/** Waits for the first line `child` prints on its stdout, and answers it. Throws, naming `what`
 *  and what the child wrote on stderr, where the child exits before it printed one, and kills the
 *  child and throws where it has printed none within the bound of `part`, the part of its row's
 *  waits it is, which the caller charged its row for, or by the time that charge answered where
 *  the row's CI step cut its limit (`boundOf`). */
function firstLine(child: Child, part: Part, what: string, { row, by }: Charged): Promise<string> {
  return during(what, async () => {
    const { ms, named } = boundOf(row, part.ms, by);
    const deadline = Date.now() + ms;
    while (!child.output.stdout.includes("\n")) {
      const code = await Promise.race([child.process.exited, sleep(200).then(() => undefined)]);
      if (code !== undefined && !child.output.stdout.includes("\n")) {
        await Promise.race([child.drained, sleep(10_000)]);
        throw new Error(`${what} exited ${code} before it served: ${child.output.stderr}`);
      }
      if (Date.now() > deadline) {
        kill(child);
        throw new Error(`${what} did not serve ${named}: ${child.output.stderr}`);
      }
    }
    return child.output.stdout.split("\n")[0] ?? "";
  });
}

/** The environment of a child of a row: this one's, with the row's own `TMPDIR` and the one
 *  provider the row names. */
export function childEnv(tmp: string, provider: { variable: ProviderVariable; value: string }) {
  const env: Record<string, string | undefined> = { ...process.env, TMPDIR: tmp };
  delete env[pgBinVariable];
  delete env[pgImageVariable];
  env[provider.variable] = provider.value;
  env[pgConnectionsVariable] = process.env[pgConnectionsVariable] ?? "100";
  return env;
}

/** One stack's Postgres as its child printed it. */
export interface Stack {
  readonly child: Child;
  readonly dir: string;
  readonly url: string;
  readonly address: string;
  /** The PID namespace the stack runs in, as `/proc/self/ns/pid` names it. */
  readonly pidNamespace: string;
}

/** Starts a stack in a child with `env`, the provider `variable` names and the run name `run`,
 *  for the row `row`, whose limit counts it (`stackStart`), and answers once it serves. */
export async function startStack(
  row: Row,
  env: Record<string, string | undefined>,
  run: string,
  variable: ProviderVariable
): Promise<Stack> {
  const part = stackStart(variable);
  const charged = charge(row, part);
  const child = spawn(stackScript, { ...env, LEFTOVER_RUN: run });
  const printed = JSON.parse(await firstLine(child, part, `stack ${run}`, charged)) as Omit<
    Stack,
    "child"
  >;
  return { child, ...printed };
}

/** Stops `stack`, of the provider `variable` names, the way its own child does, by closing its
 *  stdin, and where that did not end it within its bound (`stackStop`), kills it. The stop begins
 *  before the row `row` is charged for it, so a limit that does not count it leaves no stack
 *  running when its charge fails the row. */
export async function stopStack(
  row: Row,
  stack: Stack | undefined,
  variable: ProviderVariable
): Promise<void> {
  if (stack === undefined) return;
  const part = stackStop(variable);
  stack.child.process.stdin.end();
  const charged = charge(row, part);
  await exited(stack.child, part, `the stack in ${stack.dir}`, charged).catch(() => {});
}

/** Runs the pool's start in a child with `env`, for the row `row`, whose limit counts it as a
 *  start that finds `found` (`poolStart`), and answers its pid and what it printed. */
export async function startPool(
  row: Row,
  env: Record<string, string | undefined>,
  found: Found
): Promise<{ readonly pid: number; readonly stdout: string; readonly stderr: string }> {
  const part = poolStart(found);
  const charged = charge(row, part);
  const child = spawn(startScript, env);
  child.process.stdin.end();
  const code = await exited(child, part, "a start of the pool", charged);
  // The pipes end as it exits; their last chunks may still be on the way to `output`.
  await Promise.race([child.drained, sleep(10_000)]);
  if (code !== 0) throw new Error(`a start of the pool exited ${code}: ${child.output.stderr}`);
  return { pid: child.pid, ...child.output };
}

/** A whole start of the pool (`poolScript`) that serves, with what it printed. */
export interface PoolStart {
  readonly child: Child;
  readonly url: string;
}

/** Starts the pool in a child with `env` and the provider `variable` names, for the row `row`,
 *  whose limit counts it as a start that finds `found` (`servingStart`), and answers once its
 *  stack serves. */
export async function startServingPool(
  row: Row,
  env: Record<string, string | undefined>,
  variable: ProviderVariable,
  found: Found
): Promise<PoolStart> {
  const own: Record<string, string | undefined> = {
    ...env,
    PLAYWRIGHT_BASE_URL: "http://127.0.0.1:9",
  };
  // A run of one stack against a database the caller named starts no Postgres.
  delete own.DATABASE_URL;
  const part = servingStart(variable, found);
  const charged = charge(row, part);
  const child = spawn(poolScript, own);
  const line = await firstLine(child, part, "a start of the pool", charged);
  const { url } = JSON.parse(line) as { url?: string };
  if (url === undefined) throw new Error(`a start of the pool named no database: ${line}`);
  return { child, url };
}

/** Stops a start of the pool, of the provider `variable` names, by closing its stdin, which runs
 *  its `stopStacks`, and kills it where that did not end it within its bound (`poolStop`). The
 *  stop begins before the row `row` is charged for it, as a stack's does (`stopStack`). */
export async function stopPool(
  row: Row,
  start: PoolStart | undefined,
  variable: ProviderVariable
): Promise<void> {
  if (start === undefined) return;
  const part = poolStop(variable);
  start.child.process.stdin.end();
  const charged = charge(row, part);
  await exited(start.child, part, "a stop of the pool", charged).catch(() => {});
}

/** Runs a shell's start of a stack as `scripts/e2e/lib/leftovers.sh`'s callers run it, its
 *  `remove_leftover_postgres`, in a child with `env`, for the row `row`, whose limit counts it as a
 *  removal that finds `found` (`shellSweepOf`), and answers its exit status and its stderr. */
export async function shellSweep(
  row: Row,
  env: Record<string, string | undefined>,
  found: Found
): Promise<{ readonly status: number; readonly stderr: string }> {
  const part = shellSweepOf(found);
  const charged = charge(row, part);
  const child = launch(["bash", "-c", `. "${leftoversLib}"; remove_leftover_postgres`], env);
  child.process.stdin.end();
  const status = await exited(child, part, "a shell's start", charged);
  await Promise.race([child.drained, sleep(10_000)]);
  return { status, stderr: child.output.stderr };
}

/** Starts stack `index` of `provider`, of the provider `variable` names, in this process, for the
 *  row `row`, whose limit counts it (`ownStart`), as a wait of the row's named `what` (`during`).
 *  The provider bounds its own start, so this sets none; but where the row's CI step cut its limit
 *  (`boundOf`), the row gives the start up at its deadline and throws, naming it, and stops the
 *  stack once that start serves after all. */
export async function startOwn(
  row: Row,
  provider: StackPostgres,
  variable: ProviderVariable,
  index: number,
  what: string
): Promise<StackDatabase> {
  const { by } = charge(row, ownStart(variable));
  const { ms, named } = boundOf(row, Number.POSITIVE_INFINITY, by);
  const starting = provider.start(index);
  if (!Number.isFinite(ms)) return during(what, () => starting);
  return during(what, () =>
    Promise.race([
      starting,
      sleep(ms).then(() => {
        // A start that ran past its bound failed, so its container's log is printed as it goes.
        void starting.then((database) => database.stop(true)).catch(() => {});
        throw new Error(`${what} did not end ${named}`);
      }),
    ])
  );
}

/** Stops `database`, which a start in this process of the provider `variable` names served
 *  (`startOwn`), for the row `row`, whose limit counts it (`ownStop`). The stop begins before the
 *  row is charged for it, as a stack's does in a child (`stopStack`). The provider bounds its own
 *  stop; where the row's CI step cut its limit (`boundOf`), the row stops waiting for it at the
 *  end of its stops and throws, naming it, and the stop goes on. */
export async function stopOwn(
  row: Row,
  database: StackDatabase,
  variable: ProviderVariable
): Promise<void> {
  const what = "the stop of the row's own stack";
  const stopping = during(what, () => database.stop(false));
  // Handled here as well, so a stop that fails after a refused charge is no unhandled rejection.
  stopping.catch(() => {});
  const { by } = charge(row, ownStop(variable));
  const { ms, named } = boundOf(row, Number.POSITIVE_INFINITY, by);
  if (!Number.isFinite(ms)) return stopping;
  const late = sleep(ms).then(() => "late" as const);
  if ((await Promise.race([stopping, late])) === "late") {
    throw new Error(`${what} did not end ${named}`);
  }
}

/** Why this machine refuses a stack a PID namespace of its own, or undefined where it gives one:
 *  that takes a privilege this file borrows with `sudo -n` (`unshare --pid`), since an
 *  unprivileged user namespace is not allowed everywhere. A refusal is one line on stderr, which
 *  names `file` and says that its two rows of such a stack skip. The probe is one event of another
 *  process, `sudo -n`, which asks for nothing and answers or refuses at once, so it waits as long
 *  as a row waits for one such event (`claimEventMs`); one that has not answered by then is no
 *  refusal, and it throws, naming the probe, rather than skip the rows under a load. */
export function pidNamespaceRefusal(file: string): string | undefined {
  const probe = spawnSync("sudo", ["-n", "unshare", "--pid", "--fork", "--mount-proc", "true"], {
    encoding: "utf8",
    timeout: claimEventMs,
  });
  if (probe.status === 0) return undefined;
  if ((probe.error as NodeJS.ErrnoException | undefined)?.code === "ETIMEDOUT") {
    throw new Error(
      `${file}: \`sudo -n unshare --pid\` neither answered nor refused within ${claimEventMs / 1000} s, the time a row waits for one event of another process, so this machine's answer is not known`
    );
  }
  const why = (probe.stderr || probe.error?.message || `exit ${probe.status}`)
    .trim()
    .split("\n")[0];
  console.error(
    `${file}: skips the two rows of a stack in a PID namespace of its own: this machine refuses \`sudo -n unshare --pid\` (${why}), the permission a PID namespace of a stack's own takes here`
  );
  return why;
}

/** Starts a stack as `startStack` does, for the row `row`, with `env` and the run name `run`, in a
 *  PID namespace of its own (`unshare --pid`), which it enters through `sudo -n` and leaves for
 *  this user again (`setpriv`). The child is `sudo`'s. Its provider is the binaries provider: the
 *  rows of such a stack are its. */
export async function startNamespacedStack(
  row: Row,
  env: Record<string, string | undefined>,
  run: string
): Promise<Stack> {
  const part = stackStart(pgBinVariable);
  const charged = charge(row, part);
  // `sudo` resets the environment, so the stack's goes through `env -i`.
  const pairs = Object.entries(env)
    .filter((pair): pair is [string, string] => pair[1] !== undefined)
    .map(([key, value]) => `${key}=${value}`);
  const child = launch(
    [
      "sudo",
      "-n",
      "unshare",
      "--pid",
      "--fork",
      "--mount-proc",
      "setpriv",
      `--reuid=${process.getuid?.() ?? -1}`,
      `--regid=${process.getgid?.() ?? -1}`,
      "--init-groups",
      "--",
      "env",
      "-i",
      ...pairs,
      `LEFTOVER_RUN=${run}`,
      process.execPath,
      "--eval",
      stackScript,
    ],
    process.env
  );
  return {
    child,
    ...(JSON.parse(await firstLine(child, part, `stack ${run}`, charged)) as Omit<Stack, "child">),
  };
}

/** scripts/e2e/lib/leftovers.sh, through which each shell stack marks its Postgres and removes the
 *  Postgres of each stack that is gone. */
export const leftoversLib = fileURLToPath(
  new URL("../../../../scripts/e2e/lib/leftovers.sh", import.meta.url)
);

/** A shell stack as `scripts/e2e/lib/leftovers.sh`'s callers start one: a bash whose Postgres
 *  container carries the mark `postgres_mark` makes for the shell's own pid, and which then runs
 *  until it is killed. A SIGKILL stops nothing. */
const shellStackScript = `set -euo pipefail
. "$LEFTOVERS_LIB"
fail() { echo "FAIL: $*" >&2; exit 1; }
work=$(mktemp -d)
pg_mark=$(postgres_mark "$work" "$SHELL_PG") || fail "the mark of $SHELL_PG could not be made"
docker run -d --name "$SHELL_PG" --label "$pg_mark" -e POSTGRES_HOST_AUTH_METHOD=trust "$SHELL_IMAGE"
exec sleep infinity
`;

/** A shell stack's Postgres: the shell that owns it and the id of its container. */
export interface ShellStack {
  readonly child: Child;
  readonly id: string;
}

/** Starts a shell stack with `env` whose container is `name`, for the row `row`, whose limit
 *  counts it (`shellStackStart`), and answers once its `docker run` printed the container's id. */
export async function startShellStack(
  row: Row,
  env: Record<string, string | undefined>,
  image: string,
  name: string
): Promise<ShellStack> {
  const charged = charge(row, shellStackStart);
  const child = launch(["bash", "-c", shellStackScript], {
    ...env,
    LEFTOVERS_LIB: leftoversLib,
    SHELL_PG: name,
    SHELL_IMAGE: image,
  });
  return {
    child,
    id: (await firstLine(child, shellStackStart, `the shell stack ${name}`, charged)).trim(),
  };
}
