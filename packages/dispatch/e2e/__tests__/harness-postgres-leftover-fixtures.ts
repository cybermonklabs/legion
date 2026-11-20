/** The fixtures of the rows of `harness-postgres-leftover.test.ts` that ask Docker and Postgres
 *  and plant what a row finds: Docker's reads and the removals of what a row made, each within the
 *  time the row has left, and the marks, directories and containers that a row plants. The
 *  children those rows start and stop, and the stacks and the starts of the pool that the children
 *  run, are in `harness-postgres-provider-children.ts`. `bun test` collects only `*.test.ts` and
 *  its kin, so this module runs nowhere of its own. It holds no assertion: each check of a row is
 *  in the test file.
 *
 *  Each name a row gives is of this run (`runTag`), and each container a row made is removed by
 *  the id Docker made it under. */

import { execFile } from "node:child_process";
import { randomBytes } from "node:crypto";
import {
  chmodSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { basename, dirname, join } from "node:path";
import { setTimeout as sleep } from "node:timers/promises";
import { promisify } from "node:util";
import {
  pgBinVariable,
  pgImageVariable,
  readyMs,
  socketDirectory,
  socketFile,
} from "../harness-postgres";
import { machine, removalBounds, removeContainer } from "../harness-postgres-container-removal";
import {
  identityOf,
  markLabel,
  marksDirectory,
  type Owner,
  runDirectoryPrefix,
  stackOwner,
} from "../harness-postgres-marks";
import {
  charge,
  pgCtlMs,
  type Stack,
  startStack,
  startTimeOf,
  stopStack,
} from "./harness-postgres-provider-children";
import { boundOf, during, partOf, type Row, type Wait, waits } from "./harness-postgres-waits";
import type { ProviderVariable } from "./postgres-provider";

const execFileAsync = promisify(execFile);

/** What this file's own lines are prefixed with, as every starter in `e2e/` names itself. */
const named = "e2e/__tests__/harness-postgres-leftover-fixtures.ts";

/** What the names of this run's stacks and containers hold: random bytes, never the pid, so two
 *  runs of the rows at once, or a run after one whose container stayed, share no name. */
export const runTag = randomBytes(4).toString("hex");

/** The wait of the image provider's row for a container it made itself to serve
 *  (`startUnjudged`): the image's initialisation and its server's first answer, as the provider
 *  gives its own container `readyMs` for them. */
export const ownContainerServes = partOf("a container of the row's own that serves", {
  children: 0,
  reads: 0,
  bounds: [readyMs],
});

/** How one call of a row's fixtures of Docker or Postgres ended (`untilDeadline`): its exit status
 *  and what it printed. */
interface Called {
  readonly status: number;
  readonly stdout: string;
  readonly stderr: string;
}

/** How a fixture call that its row's time did not let end ended (`untilDeadline`): "late" where
 *  that time ran out while the call ran, which stopped it, and "unstarted" where it had run out
 *  before the call, which then started nothing. */
type Unended = "late" | "unstarted";

/** Runs `program` with `args`, a call of Docker or Postgres that sets up the row `row`, or reads
 *  it, for as long as the row still has time for: until `by`, the row's deadline but for a removal
 *  of what it made (`Row.cleanup`), on the machine's clock, as a start's removal runs its `docker rm`
 *  (`machine` of `e2e/harness-postgres-container-removal.ts`). It sets no time of its own: a shared
 *  daemon took 133 s over one `docker create` on 2026-11-19 while it answered every other call, and
 *  a fixed bound of 120 s gave that call up, and left the container it made. Where `by` has passed
 *  already, it starts nothing: a call with no time at all is no call that ran out its time. The
 *  call is a wait of the row (`during`). Answers how the call ended (`Called`), or how `by` ended
 *  it (`Unended`). */
async function untilDeadline(
  row: Row,
  program: string,
  args: readonly string[],
  by = row.deadline
): Promise<Called | Unended> {
  if (machine.now() >= by) return "unstarted";
  return during(`${program} ${args.join(" ")}`, async () => {
    const stop = new AbortController();
    const ended = new AbortController();
    void machine.sleep(Math.max(0, by - machine.now()), ended.signal).then(() => {
      if (!ended.signal.aborted) stop.abort();
    });
    try {
      const { stdout, stderr } = await execFileAsync(program, args, {
        signal: stop.signal,
        encoding: "utf8",
        maxBuffer: 64 * 1024 * 1024,
      });
      return { status: 0, stdout, stderr };
    } catch (error) {
      if (stop.signal.aborted) return "late";
      const failed = error as { code?: unknown; stdout?: unknown; stderr?: unknown };
      const stderr = typeof failed.stderr === "string" ? failed.stderr : "";
      return {
        status: typeof failed.code === "number" ? failed.code : 1,
        stdout: typeof failed.stdout === "string" ? failed.stdout : "",
        stderr: stderr === "" && error instanceof Error ? error.message : stderr,
      };
    } finally {
      ended.abort();
    }
  });
}

/** `ms` in seconds, to a tenth, as the lines of this file give a time. */
function seconds(ms: number): string {
  return (ms / 1000).toFixed(1);
}

/** What used up the time of the row `row` that ended at `by` (`waits`): the wait of the row that
 *  ran at `by`, the one of them that ended last where several did, with when it began and ended
 *  from the row's start; or, where none ran then, the last that ended before it, after which the
 *  time went in a wait the row does not name. */
function usedUp(row: Row, by: number): string {
  const start = row.deadline - row.ms;
  const mine = waits.filter((wait) => wait.began >= start);
  const endOf = (wait: Wait) => wait.ended ?? Number.POSITIVE_INFINITY;
  const ran = mine
    .filter((wait) => wait.began <= by && endOf(wait) > by)
    .sort((a, b) => endOf(b) - endOf(a))[0];
  if (ran !== undefined) {
    const to = ran.ended === undefined ? "on" : `to ${seconds(ran.ended - start)} s`;
    return `during ${ran.what}, from ${seconds(ran.began - start)} s ${to} of the row`;
  }
  const before = mine.filter((wait) => endOf(wait) <= by).at(-1);
  return before?.ended === undefined
    ? "in a wait the row does not name"
    : `after ${before.what}, which ended at ${seconds(before.ended - start)} s of the row, in a wait the row does not name`;
}

/** The one line, and the error, of a fixture call `call` of `row` that its time did not let end
 *  (`Unended`), where that time ended at `by`: the row's deadline, or its time for its removals
 *  (`Row.cleanup`). A call that ran out that time names the call, the row's time, what set it
 *  where the row's CI step cut it (`Row.cut`), and what the removal of what it made did
 *  (`removed`); a call that the time found already passed names the call, how long before it the
 *  time had run out, and the wait that used it up (`usedUp`). */
function ranOut(row: Row, call: string, how: Unended, by = row.deadline, removed = ""): Error {
  const waits = `the ${row.ms / 1000} s the row waits in all${row.cut === undefined ? "" : `, which ${row.cut} set`}`;
  const time = by === row.deadline ? waits : "the row's time for its removals";
  const line =
    how === "late"
      ? `${named}: ${call} did not end within ${waits}, so the row stopped it${removed}`
      : `${named}: ${call} did not start: ${time} had run out ${seconds(machine.now() - by)} s before it, ${usedUp(row, by)}`;
  console.error(line);
  return new Error(line);
}

/** `psql` of `url` with the one statement `sql`, read within the time `row` has left
 *  (`untilDeadline`): answers how it ended, and throws naming the read where the row's deadline
 *  comes first. */
export async function psql(row: Row, url: string, sql: string): Promise<Called> {
  const read = await untilDeadline(row, "psql", [url, "-XAtc", sql]);
  if (typeof read === "string") throw ranOut(row, `psql ${url} -XAtc '${sql}'`, read);
  return read;
}

/** Whether the Postgres at `url` answers `select 1`, read within the time `row` has left
 *  (`psql`). */
export async function answers(row: Row, url: string): Promise<boolean> {
  const read = await psql(row, url, "select 1");
  return read.status === 0 && read.stdout.trim() === "1";
}

/** Each postmaster that serves a data directory of the binaries provider's run directory `dir`,
 *  by the pid its `postmaster.pid` names and the start `/proc` gives that pid now. */
export function postmastersOf(dir: string): { pid: number; start: string }[] {
  return ["worker-0", "template"].flatMap((stack) => {
    let text: string;
    try {
      text = readFileSync(join(dir, stack, "pgdata", "postmaster.pid"), "utf8");
    } catch {
      return [];
    }
    const pid = Number.parseInt(text, 10);
    const start = startTimeOf(pid);
    return start === "" ? [] : [{ pid, start }];
  });
}

/** Removes what the `docker` call of `args` made, which its row stopped at the row's deadline
 *  (`docker`), by the row's time for its removals (`Row.cleanup`): the container a `create` or a
 *  `run` names (`--name`), as Docker lists it by that name and by its `--label`'s key, each by its
 *  id (`removeContainer`), or the image a `commit` names, its last argument. Answers what it did,
 *  for the call's line. */
async function removeMade(row: Row, args: readonly string[]): Promise<string> {
  const by = row.cleanup;
  const left = () => Math.max(0, by - machine.now());
  if (args[0] === "commit") {
    const image = args.at(-1) ?? "";
    const removal = await untilDeadline(row, "docker", ["image", "rm", "--force", image], by);
    if (typeof removal === "string") {
      return `, and the removal of the image ${image} it made did not ${removal === "late" ? "end" : "start"}`;
    }
    return removal.status === 0
      ? `, and removed the image ${image} it made`
      : `, and left the image ${image} it made: ${removal.stderr.trim()}`;
  }
  const name = args[args.indexOf("--name") + 1];
  if ((args[0] !== "create" && args[0] !== "run") || !args.includes("--name") || !name) return "";
  const filters = ["--filter", `name=^/?${name}$`];
  const label = args.includes("--label") ? args[args.indexOf("--label") + 1]?.split("=")[0] : "";
  if (label) filters.push("--filter", `label=${label}`);
  const listed = await untilDeadline(
    row,
    "docker",
    ["ps", "--all", "--quiet", "--no-trunc", ...filters],
    by
  );
  if (typeof listed === "string" || listed.status !== 0) {
    return `, and could not list the container ${name} it may have made`;
  }
  const ids = listed.stdout.split("\n").filter(Boolean);
  if (ids.length === 0) return `, and it had made no container ${name}`;
  const said: string[] = [];
  for (const id of ids) {
    const removal = await removeContainer(id, `container ${id.slice(0, 12)} (${name})`, {
      answerMs: left(),
      boundMs: left(),
      watchMs: removalBounds.watchMs,
    });
    said.push(
      removal.kind === "failed"
        ? `left container ${id.slice(0, 12)} (${name}) it made: ${removal.reason}`
        : `removed container ${id.slice(0, 12)} (${name}) it made`
    );
  }
  return `, and ${said.join(", and ")}`;
}

/** `docker` with `args`, as a call that sets up or reads the row `row` runs it (`untilDeadline`):
 *  answers what it printed, and throws where it fails, with Docker's reason. A call that makes a
 *  container or an image (`create`, `run`, `commit`) charges the row for it (`Row.made`), whose
 *  limit counts it. Where the row's deadline comes first, the row stops the call, removes what it
 *  made (`removeMade`), and then throws, naming the call and that removal on one line. */
export async function docker(row: Row, ...args: string[]): Promise<string> {
  const call = `docker ${args.join(" ")}`;
  if (args[0] === "create" || args[0] === "run" || args[0] === "commit") row.made(call);
  const ended = await untilDeadline(row, "docker", args);
  // A call that started nothing made nothing.
  if (typeof ended === "string") {
    const removed = ended === "late" ? await removeMade(row, args) : "";
    throw ranOut(row, call, ended, row.deadline, removed);
  }
  if (ended.status !== 0) throw new Error(`${call}: ${ended.stderr}`);
  return ended.stdout;
}

/** Whether Docker has the object `name` of `kind`, read with `docker <kind> inspect` within the
 *  time `row` has left (`untilDeadline`): false only where Docker says it has no such object, and
 *  a throw where the read fails otherwise or the row's deadline comes first, so a row never reads
 *  a failed read as a removal. */
export async function exists(
  row: Row,
  kind: "container" | "volume",
  name: string
): Promise<boolean> {
  const call = `docker ${kind} inspect ${name}`;
  const read = await untilDeadline(row, "docker", [kind, "inspect", name]);
  if (typeof read === "string") throw ranOut(row, call, read);
  if (read.status === 0) return true;
  if (/no such (container|volume|object)/i.test(read.stderr)) return false;
  throw new Error(`${call}: ${read.stderr}`);
}

/** Whether a container of this name exists, running or not (`exists`). */
export function containerExists(row: Row, name: string): Promise<boolean> {
  return exists(row, "container", name);
}

/** The volumes of the container `name`: the image's data volume, which holds its stack's data. */
export async function volumesOf(row: Row, name: string): Promise<string[]> {
  const format = '{{range .Mounts}}{{if eq .Type "volume"}}{{.Name}} {{end}}{{end}}';
  return (await docker(row, "container", "inspect", "--format", format, name))
    .trim()
    .split(/\s+/)
    .filter(Boolean);
}

/** The ids of the containers whose mark names `run`, the run directory of a stack this row
 *  started, as Docker lists them within the time `row` has left, until `by` (`untilDeadline`). A
 *  listing that fails, or that the row's deadline stops, throws, so no row reads it as one that
 *  lists none. */
export async function containersOf(row: Row, run: string, by = row.deadline): Promise<string[]> {
  const args = [
    "ps",
    "--all",
    "--no-trunc",
    "--filter",
    `label=${markLabel}`,
    "--format",
    `{{.ID}} {{.Label "${markLabel}"}}`,
  ];
  const listed = await untilDeadline(row, "docker", args, by);
  if (typeof listed === "string") throw ranOut(row, `docker ${args.join(" ")}`, listed, by);
  if (listed.status !== 0) throw new Error(`docker ${args.join(" ")}: ${listed.stderr}`);
  return listed.stdout.split("\n").flatMap((line) => {
    const space = line.indexOf(" ");
    if (space === -1) return [];
    let mark: unknown;
    try {
      mark = JSON.parse(line.slice(space + 1));
    } catch {
      return [];
    }
    const ofRun = typeof mark === "object" && mark !== null && "run" in mark && mark.run === run;
    return ofRun ? [line.slice(0, space)] : [];
  });
}

/** Removes each container of `ids`, by its id, all at once, each as a start removes one
 *  (`removeContainer`): however long Docker takes over it while it answers each ask, within the
 *  start's own bound, which the row's limit counts once among the waits of its `finally`
 *  (`Waits.ends`). Once each has ended, throws naming each removal that Docker gave up or
 *  refused, with why: one failed removal keeps no other container. A container that is gone
 *  already is no failure, nor is one that another start of the machine removed first. A daemon
 *  that many sessions shared took 17 and 21 minutes over one removal on 2026-11-19, which a
 *  removal with no bound of its own carried past the row's limit. */
export async function removeIds(ids: readonly string[]): Promise<void> {
  const removals = await Promise.all(
    ids.map((id) => {
      const what = `container ${id.slice(0, 12)}`;
      return during(`the removal of ${what}`, () => removeContainer(id, what));
    })
  );
  const failed = removals.flatMap((removal, index) =>
    removal.kind === "failed" ? [`${ids[index]}: ${removal.reason}`] : []
  );
  if (failed.length > 0) {
    throw new Error(`${named}: what the row made is not removed: ${failed.join("; ")}`);
  }
}

/** Removes the image `name`, which a row made, within the start's own bound for the removal of a
 *  container, which the row's limit counts among the waits of its `finally` (`Waits.ends`): Docker
 *  gives a removal no bound of its own. */
export async function removeImage(name: string): Promise<void> {
  const args = ["image", "rm", "--force", name];
  await during(`docker ${args.join(" ")}`, () =>
    machine.docker(args, { signal: new AbortController().signal, timeout: removalBounds.boundMs })
  ).catch((error: unknown) => {
    throw new Error(
      `docker ${args.join(" ")}: ${error instanceof Error ? error.message : String(error)}`
    );
  });
}

/** Removes the container `id`, which a row made and its line names `what`, as a start removes one
 *  (`removeContainer`): however long Docker takes over it while it answers each ask, within the
 *  start's own bound, which the row's limit counts among the waits of its `finally`
 *  (`Waits.ends`). Throws, naming the container and why, where the removal gives it up. */
export async function removeWithin(id: string, what: string): Promise<void> {
  const removal = await during(`the removal of ${what}`, () => removeContainer(id, what));
  if (removal.kind === "failed") {
    throw new Error(`${named}: ${what} is not removed: ${removal.reason}`);
  }
}

/** Removes each container whose mark names `run`, the run directory of a stack the row `row`
 *  started, by the id Docker lists it under, listed by the row's time for its removals
 *  (`Row.cleanup`): a container's name is no one stack's alone. */
export async function removeContainersOf(row: Row, run: string): Promise<void> {
  await removeIds(await containersOf(row, run, row.cleanup));
}

/** Stops the postmaster of the cluster in `data` with `pg_ctl` of `bin`, as a stack's stop does
 *  (`-m immediate -w`), by the row's time for its removals (`Row.cleanup`): `pg_ctl`'s own
 *  wait for the stop, `PGCTLTIMEOUT`, bounds it, and no fixed time of this file's. Throws naming
 *  the stop where that room runs out first. A stop that fails is a postmaster that is gone. */
export async function stopCluster(row: Row, bin: string, data: string): Promise<void> {
  const args = ["-D", data, "-m", "immediate", "-w", "stop"];
  const stopped = await untilDeadline(row, join(bin, "pg_ctl"), args, row.cleanup);
  if (typeof stopped === "string") {
    throw ranOut(row, `${join(bin, "pg_ctl")} ${args.join(" ")}`, stopped, row.cleanup);
  }
}

/** Removes what a stack the row `row` started left, by the paths it recorded for it: its
 *  container, by the id its mark is listed under, or its postmasters, then its directories. */
export async function removeLeft(
  row: Row,
  stack: Stack | undefined,
  provider: { variable: ProviderVariable; value: string }
): Promise<void> {
  if (stack === undefined) return;
  if (provider.variable === pgImageVariable) {
    await removeContainersOf(row, stack.dir);
    rmSync(dirname(stack.address), { recursive: true, force: true });
  } else {
    for (const data of [
      join(stack.dir, "worker-0", "pgdata"),
      join(stack.dir, "template", "pgdata"),
    ]) {
      if (existsSync(join(data, "postmaster.pid"))) await stopCluster(row, provider.value, data);
    }
    rmSync(stack.address, { recursive: true, force: true });
  }
}

/** What `removeLeft` waits for, in ms, for a stack of the provider `variable` names, which a row's
 *  limit counts among the waits of its `finally` (`Waits.ends`): the removal of the stack's
 *  containers at once, each within the start's own bound (`removeIds`), or the stops of its two
 *  postmasters, each within `pgCtlMs` (`stopCluster`). */
export function leftRemovalMs(variable: ProviderVariable): number {
  return variable === pgImageVariable ? removalBounds.boundMs : 2 * pgCtlMs;
}

/** Runs each removal of a row's `finally`, every one even where another fails, and throws naming
 *  each that failed, with what it said. */
export async function removeAll(...removals: (() => unknown)[]): Promise<void> {
  const failed: string[] = [];
  for (const removal of removals) {
    try {
      await removal();
    } catch (error) {
      failed.push(error instanceof Error ? error.message : String(error));
    }
  }
  if (failed.length > 0) throw new Error(`the row's removals failed: ${failed.join("; ")}`);
}

/** The owners a row writes into marks, each built from this process: this process, which runs;
 *  its pid with another start, which is a process that took over the pid of a gone owner; another
 *  boot; another PID namespace; another time namespace; and none, which is a stack's where there
 *  is no /proc. */
export function owners() {
  const runs = stackOwner();
  if (runs === null) throw new Error("this machine has no /proc, so no start judges an owner");
  return {
    runs,
    gone: { ...runs, start: "0" },
    otherBoot: { ...runs, boot: "00000000-0000-0000-0000-000000000000" },
    otherNamespace: { ...runs, pidNamespace: "pid:[1]" },
    otherTime: { ...runs, timeNamespace: "time:[1]" },
    noProc: null,
  };
}

/** A Postgres of the row's own that serves under a mark the start cannot judge, of another boot,
 *  with the name a start's line names it by and its own stop. */
export interface Unjudged {
  readonly url: string;
  readonly named: string;
  stop(): Promise<void> | void;
}

/** Starts a Postgres that serves under a mark of another boot, for the row `row`. The binaries
 *  provider's is a stack started as `startStack` starts one, whose mark is then written again with
 *  another boot's owner; the start names its mark file. The image provider's is a container of the
 *  row's own, made as the provider makes one, whose label is such a mark from its create on, in a
 *  socket directory of the run directory `run`; the start names it by its id. Its calls of Docker
 *  and Postgres take the time the row has left (`docker`), and its wait for the container to serve
 *  is a part of the row's waits (`ownContainerServes`). */
export async function startUnjudged(
  row: Row,
  env: Record<string, string | undefined>,
  provider: { variable: ProviderVariable; value: string },
  run: string,
  beside: string
): Promise<Unjudged> {
  if (provider.variable === pgBinVariable) {
    const stack = await startStack(row, env, run, pgBinVariable);
    const markFile = join(stack.dir, marksDirectory, "worker-0.json");
    const mark = JSON.parse(readFileSync(markFile, "utf8")) as Record<string, unknown>;
    writeFileSync(markFile, JSON.stringify({ ...mark, owner: owners().otherBoot }));
    return {
      url: stack.url,
      named: markFile,
      stop: async () => {
        await stopStack(row, stack, pgBinVariable);
        await removeLeft(row, stack, provider);
      },
    };
  }
  const { by } = charge(row, ownContainerServes);
  // In the run directory, or in /tmp where a long TMPDIR leaves no room there for the socket's
  // path, as the provider puts each of its own.
  const own = socketDirectory(beside, "pg-9", join("s", socketFile));
  const sockets = join(own, "s");
  mkdirSync(sockets, { mode: 0o700 });
  const name = `dispatch-e2e-pg-${run}-9`;
  const mark = {
    owner: owners().otherBoot,
    run: beside,
    identity: identityOf(beside),
    sockets: own,
  };
  let id: string;
  try {
    id = (
      await docker(
        row,
        "run",
        "--detach",
        "--name",
        name,
        "--label",
        `${markLabel}=${JSON.stringify({ ...mark, container: name })}`,
        "--user",
        `${process.getuid?.()}:${process.getgid?.()}`,
        "--mount",
        `type=bind,source=${sockets},target=/var/run/postgresql`,
        "--env",
        "POSTGRES_HOST_AUTH_METHOD=trust",
        provider.value,
        "-c",
        "listen_addresses="
      )
    ).trim();
  } catch (error) {
    // The socket directory is in /tmp where the row's TMPDIR is long, so the row's own removal of
    // its TMPDIR does not reach it.
    rmSync(own, { recursive: true, force: true });
    throw error;
  }
  const url = `postgres://postgres@/postgres?host=${encodeURIComponent(sockets)}&sslmode=disable`;
  // The image's first server is its initialisation's, which it stops before its last one starts.
  const initialised = async () => {
    const logs = await untilDeadline(row, "docker", ["logs", id]);
    if (typeof logs === "string") throw ranOut(row, `docker logs ${id}`, logs);
    return `${logs.stdout}${logs.stderr}`.includes("init process complete");
  };
  // The container by the id Docker made it under, and then its socket directory, which is in /tmp
  // and not in the row's TMPDIR where that TMPDIR is long.
  const stop = async () => {
    await removeAll(
      () => removeIds([id]),
      () => rmSync(own, { recursive: true, force: true })
    );
  };
  const serves = boundOf(row, ownContainerServes.ms, by);
  const deadline = Date.now() + serves.ms;
  try {
    while (!((await initialised()) && (await answers(row, url)))) {
      if (Date.now() > deadline) {
        throw new Error(`the container ${name} did not serve ${serves.named}`);
      }
      await sleep(500);
    }
  } catch (error) {
    // A read the row's deadline stopped leaves the container this row made too.
    await stop();
    throw error;
  }
  return { url, named: `container ${id.slice(0, 12)}`, stop };
}

/** What a binaries provider's stack left in `run`, as its mark names it: a data directory and a
 *  socket directory, each holding a file, and the mark. No server serves from it, so a start that
 *  judges its owner gone removes the directories and the mark alone. `text` replaces the mark
 *  where a row writes one that is no mark. */
export function leaveCluster(
  run: string,
  index: number,
  bin: string,
  owner: Owner | null,
  options: { sockets?: string; text?: string } = {}
) {
  const data = join(run, `worker-${index}`, "pgdata");
  mkdirSync(data, { recursive: true });
  writeFileSync(join(data, "PG_VERSION"), "18\n");
  const sockets = options.sockets ?? mkdtempSync(join(run, `pg-${index}-`));
  writeFileSync(join(sockets, "socket"), "");
  mkdirSync(join(run, marksDirectory), { recursive: true });
  const mark = join(run, marksDirectory, `worker-${index}.json`);
  writeFileSync(
    mark,
    options.text ??
      JSON.stringify({ owner, run, identity: identityOf(run), sockets, postmaster: { bin, data } })
  );
  return { data, sockets, mark, left: () => [data, sockets, mark].map(existsSync) };
}

/** The name of stack `index`'s container in the run directory `run` a row made: of this run
 *  (`runTag`) and of that directory, so no other row and no other run holds it. */
export function plantedName(run: string, index: number): string {
  return `dispatch-e2e-pg-${basename(run).slice(runDirectoryPrefix.length)}-${runTag}-${index}`;
}

/** A container an image provider's stack left, made and never started, under a name of this
 *  row's own (`plantedName`): a start judges it by its label alone, which is its mark unless
 *  `label` names another. `made` collects the id Docker made it under, for the row's removal. Its
 *  `docker create` takes the time the row `row` has left, which counts it (`docker`). */
export async function leaveContainer(
  row: Row,
  image: string,
  made: string[],
  run: string,
  index: number,
  owner: Owner | null,
  sockets: string,
  label?: string
) {
  const name = plantedName(run, index);
  const mark =
    label ?? JSON.stringify({ owner, run, identity: identityOf(run), sockets, container: name });
  const id = (
    await docker(row, "create", "--name", name, "--label", `${markLabel}=${mark}`, image)
  ).trim();
  made.push(id);
  return { name, id: id.slice(0, 12), sockets };
}

/** The mark file in `run` of stack `index`'s container `container`, whose socket directory is
 *  `sockets`, as the image provider writes it beside the container's label. Answers its path. */
export function leaveContainerMark(
  run: string,
  index: number,
  owner: Owner | null,
  sockets: string,
  container: string
): string {
  mkdirSync(join(run, marksDirectory), { recursive: true });
  const mark = join(run, marksDirectory, `worker-${index}.json`);
  writeFileSync(
    mark,
    JSON.stringify({ owner, run, identity: identityOf(run), sockets, container })
  );
  return mark;
}

/** A container an image provider's stack left, as `leaveContainer` makes one, but with no label
 *  that Docker lists: its mark is a line of `docker ps`'s listing (`listing`) that the stand-in
 *  `docker` of `soleListing` gives the starts under test alone, so no other start of the machine
 *  removes it first. `made` collects the id Docker made it under, for the row's removal. Its
 *  `docker create` takes the time the row `row` has left, which counts it (`docker`). */
export async function leaveUnlistedContainer(
  row: Row,
  image: string,
  made: string[],
  run: string,
  index: number,
  owner: Owner | null
) {
  const name = plantedName(run, index);
  const sockets = mkdtempSync(join(run, `pg-${index}-`));
  const id = (await docker(row, "create", "--name", name, image)).trim();
  made.push(id);
  const mark = { owner, run, identity: identityOf(run), sockets, container: name };
  return {
    name,
    id: id.slice(0, 12),
    fullId: id,
    sockets,
    listing: `${id} ${name} ${JSON.stringify(mark)}`,
  };
}

/** A directory in `tmp` with a stand-in `docker` for the starts under test, answered as `bin`: a
 *  listing of the marks is the real one with each line of `listings` added
 *  (`leaveUnlistedContainer`), and every other call runs the lines of bash `staged` gives for the
 *  real `docker`'s path, with which a row stages a call, and then the real `docker`. */
export function soleListing(
  tmp: string,
  listings: readonly string[],
  staged: (real: string) => readonly string[] = () => []
): { readonly bin: string } {
  const real = Bun.which("docker");
  if (real === null) throw new Error("no docker on PATH");
  const bin = mkdtempSync(join(tmp, "bin-"));
  writeFileSync(join(bin, "listed"), `${listings.join("\n")}\n`);
  writeFileSync(
    join(bin, "docker"),
    [
      "#!/usr/bin/env bash",
      `if [ "$1" = ps ] && [[ " $* " == *" label=${markLabel} "* ]]; then`,
      `  ${JSON.stringify(real)} "$@" || exit`,
      `  exec cat ${JSON.stringify(join(bin, "listed"))}`,
      "fi",
      ...staged(real),
      `exec ${JSON.stringify(real)} "$@"`,
      "",
    ].join("\n")
  );
  chmodSync(join(bin, "docker"), 0o755);
  return { bin };
}

/** A socket directory that no start can remove: it holds a directory of mode 0500 with a file in
 *  it, which its owner cannot unlink. The row gives the mode back before it removes its files. */
export function heldSockets(parent: string, index: number): string {
  const sockets = mkdtempSync(join(parent, `pg-${index}-`));
  mkdirSync(join(sockets, "held"));
  writeFileSync(join(sockets, "held", "socket"), "");
  chmodSync(join(sockets, "held"), 0o500);
  return sockets;
}
