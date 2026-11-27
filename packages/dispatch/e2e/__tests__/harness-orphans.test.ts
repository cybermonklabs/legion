/** What is left of a harness whose parent dies without stopping it: nothing.
 *
 *  Every listener the harness starts is the leader of a process group of its own
 *  (`Supervisor`, e2e/local-instance-support.ts), so a stop reaches it and what it starts in turn.
 *  A parent that is killed - SIGKILL, an out-of-memory kill, a crashed runner - runs no stop, so
 *  its reaper (`Reaper`) ends what it started: each listener - fake-envoy, fake-github,
 *  fake-broker, the plain-HTTP proxy and the dev OIDC issuer - and each stack's Postgres, a
 *  container or a cluster the run started outside any process group of its own. Each row starts
 *  the real thing in a parent process of its own, kills that parent with SIGKILL, and reads what
 *  it started: none of it may survive.
 *
 *  The listeners' harness is a deployed run's (`PLAYWRIGHT_BASE_URL`), which starts the fake Envoy
 *  and the plain-HTTP proxy and no Dispatch server, so the row needs no database and no Go build.
 *  Each is started by `startHarness` exactly as a local run starts it. The Postgres row starts one
 *  stack's server from the provider this run names (`DISPATCH_E2E_PG_IMAGE` or
 *  `DISPATCH_E2E_PG_BIN`, e2e/harness-postgres.ts), as the pool starts each stack's. */
import { afterEach, expect, test } from "bun:test";
import { connect } from "node:net";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

import { harnessVariables, stacksVariable } from "../harness-ports";
import { pgBinVariable, pgConnectionsVariable, pgImageVariable } from "../harness-postgres";

const makeDirectory = temporaryDirectories("harness-orphans");
const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));
const harnessStart = fileURLToPath(new URL("../harness-start.ts", import.meta.url));
const harnessPostgres = fileURLToPath(new URL("../harness-postgres.ts", import.meta.url));

/** A hang guard: a cold `bun` start of two listeners takes a few seconds, and the reaping that
 *  follows the kill is bounded below at 15 s. */
const ROW_MS = 60_000;
/** The Postgres row's hang guard: a container's first start, which pulls nothing here but runs
 *  the image's initdb, and the reaper's removal after the kill. */
const POSTGRES_ROW_MS = 120_000;

/** True while any process of group `pgid` is alive: signal 0 to the group reaches every member. */
function groupAlive(pgid: number): boolean {
  try {
    process.kill(-pgid, 0);
    return true;
  } catch {
    return false;
  }
}

/** Polls `ready` until it holds or `ms` pass: what this waits for is another process's write or
 *  the kernel reaping a killed one, which raise no event here. */
async function eventually(ready: () => boolean | Promise<boolean>, ms: number): Promise<boolean> {
  const deadline = Date.now() + ms;
  while (Date.now() < deadline) {
    if (await ready()) return true;
    await Bun.sleep(50);
  }
  return ready();
}

/** The processes whose parent is `pid`, as `pgrep -P` lists them. */
function childrenOf(pid: number): number[] {
  const listed = Bun.spawnSync(["pgrep", "-P", String(pid)])
    .stdout.toString()
    .trim();
  return listed === "" ? [] : listed.split("\n").map(Number);
}

/** Groups a failing row found alive, killed after it so the row leaks nothing itself. */
const survivors: number[] = [];
afterEach(() => {
  for (const pgid of survivors.splice(0)) {
    try {
      process.kill(-pgid, "SIGKILL");
    } catch {
      // ESRCH: the group is gone.
    }
  }
});

test(
  "a harness whose parent is killed leaves none of its listeners running",
  async () => {
    const pidsFile = join(makeDirectory(), "pids");
    // The parent's environment: this one's, with no port and no stack manifest a lane set, so
    // every listener binds a port the OS chooses, and a deployed run's base URL, which nothing
    // here dials.
    const env: Record<string, string | undefined> = {
      ...process.env,
      PLAYWRIGHT_BASE_URL: "http://127.0.0.1:9",
      [stacksVariable]: undefined,
    };
    for (const variable of Object.values(harnessVariables)) env[variable] = undefined;
    const parent = Bun.spawn(
      [
        "bun",
        "-e",
        `import { writeFileSync } from "node:fs";
         import { startHarness } from ${JSON.stringify(harnessStart)};
         const harness = await startHarness();
         writeFileSync(${JSON.stringify(pidsFile)},
           harness.processes.map((started) => started.child.pid).join("\\n") + "\\n");
         setInterval(() => {}, 1 << 30);`,
      ],
      { cwd: dispatchPackage, env, stderr: "ignore", stdout: "ignore" }
    );
    const written = await eventually(
      async () =>
        /\n$/u.test(
          await Bun.file(pidsFile)
            .text()
            .catch(() => "")
        ),
      ROW_MS / 2
    );
    if (!written) {
      parent.kill("SIGKILL");
      throw new Error("the parent started no harness: it wrote no pids");
    }
    const pids = (await Bun.file(pidsFile).text()).trim().split("\n").map(Number);
    survivors.push(...pids);
    expect(pids.length).toBe(2);
    expect(pids.every(groupAlive)).toBe(true);

    parent.kill("SIGKILL");
    await parent.exited;

    await eventually(() => !pids.some(groupAlive), 15_000);
    expect(pids.filter(groupAlive)).toEqual([]);
  },
  // The row drives a parent it kills and its reaper: a cold bun start of two listeners, then a reap bounded below at 15 s.
  ROW_MS
);

// The binaries provider's clusters are no process group and no container: the reaper stops each
// with its own `pg_ctl` and data directory, and leaves alone one its starter already stopped and
// took back. A `pg_ctl` that records how it was run stands in for the real one, so this row
// holds on any machine, Postgres binaries or none; the Postgres row above runs the real server.
test(
  "a reaper whose starter is killed stops each cluster it still holds, and none it gave back",
  async () => {
    const dir = makeDirectory();
    const calls = join(dir, "calls");
    const pgCtl = join(dir, "pg_ctl");
    await Bun.write(
      pgCtl,
      `#!/usr/bin/env bash\nprintf '%s\\n' "$*" >> ${JSON.stringify(calls)}\n`
    );
    await Bun.spawn(["chmod", "+x", pgCtl]).exited;
    const ready = join(dir, "ready");
    const reaper = fileURLToPath(new URL("../local-instance-support.ts", import.meta.url));
    const parent = Bun.spawn(
      [
        "bun",
        "-e",
        `import { writeFileSync } from "node:fs";
         import { Reaper } from ${JSON.stringify(reaper)};
         const reaper = new Reaper();
         reaper.watch({ cluster: { pgCtl: ${JSON.stringify(pgCtl)}, data: "/data/held" } });
         reaper.watch({ cluster: { pgCtl: ${JSON.stringify(pgCtl)}, data: "/data/given back" } });
         reaper.forget({ cluster: { pgCtl: ${JSON.stringify(pgCtl)}, data: "/data/given back" } });
         writeFileSync(${JSON.stringify(ready)}, "ready\\n");
         setInterval(() => {}, 1 << 30);`,
      ],
      { stderr: "inherit", stdout: "ignore" }
    );
    try {
      expect(await eventually(async () => await Bun.file(ready).exists(), ROW_MS / 2)).toBe(true);
      // The reaper is the parent's one child, the leader of a session and group of its own, and it
      // exits only once every stop it makes has ended, by itself within a minute: its group's end
      // is the moment every call it was going to make is in.
      const reapers = childrenOf(parent.pid);
      const [reaperGroup] = reapers;
      if (reapers.length !== 1 || reaperGroup === undefined) {
        throw new Error(`the parent should have one child, its reaper; it has ${reapers.length}`);
      }
      parent.kill("SIGKILL");
      await parent.exited;
      expect(await eventually(() => !groupAlive(reaperGroup), 15_000)).toBe(true);
      expect((await Bun.file(calls).text()).trim().split("\n")).toEqual([
        "-D /data/held -m immediate -w stop",
      ]);
    } finally {
      parent.kill("SIGKILL");
    }
  },
  // The row drives a starter it kills and its reaper, whose stops each end by themselves within a minute.
  ROW_MS
);

// A foreground step (`Supervisor.run`: dev-postgres.sh, the web build) ends long before the run
// does, and the id of its process group is then free for the system to give another process. So
// the step is given back to the reaper as it ends, and a reaper whose starter is killed signals
// no group of a step that already ended. A process left holding the step's group once the step's
// leader has exited stands in for one the system gave that id to, and records any SIGTERM it is
// sent; a process the supervisor still runs is the reaper's to end, so the row also sees the
// reaper end what it holds.
test(
  "a reaper whose starter is killed signals no group of a foreground step that already ended",
  async () => {
    const dir = makeDirectory();
    const stepGroup = join(dir, "step-group");
    const signals = join(dir, "signals");
    const kept = join(dir, "kept");
    const standIn = join(dir, "stand-in");
    await Bun.write(
      standIn,
      `#!/usr/bin/env bash\ntrap 'echo TERM >> "$1"; exit 0' TERM\nsleep 60 &\nwait\n`
    );
    await Bun.spawn(["chmod", "+x", standIn]).exited;
    const step = `echo $$ > ${JSON.stringify(stepGroup)}; ${JSON.stringify(standIn)} ${JSON.stringify(signals)} &`;
    const support = fileURLToPath(new URL("../local-instance-support.ts", import.meta.url));
    const parent = Bun.spawn(
      [
        "bun",
        "-e",
        `import { writeFileSync } from "node:fs";
         import { Supervisor } from ${JSON.stringify(support)};
         const supervisor = new Supervisor();
         await supervisor.run(["bash", "-c", ${JSON.stringify(step)}], "/");
         const held = supervisor.start("held", ["sleep", "60"], "/");
         writeFileSync(${JSON.stringify(kept)}, held.child.pid + "\\n");
         setInterval(() => {}, 1 << 30);`,
      ],
      { stderr: "inherit", stdout: "ignore" }
    );
    try {
      const written = await eventually(
        async () =>
          /\n$/u.test(
            await Bun.file(kept)
              .text()
              .catch(() => "")
          ),
        ROW_MS / 2
      );
      if (!written) throw new Error("the parent ran no step: it wrote no pid of the held process");
      const group = Number((await Bun.file(stepGroup).text()).trim());
      const held = Number((await Bun.file(kept).text()).trim());
      survivors.push(group, held);
      expect(groupAlive(group)).toBe(true);
      expect(groupAlive(held)).toBe(true);
      // The parent's other child is its reaper, the leader of a session and group of its own,
      // which exits only once it has signalled every group it holds and seen them end.
      const reapers = childrenOf(parent.pid).filter((pid) => pid !== held);
      const [reaperGroup] = reapers;
      if (reapers.length !== 1 || reaperGroup === undefined) {
        throw new Error(`the parent should have one child besides the held process: ${reapers}`);
      }

      parent.kill("SIGKILL");
      await parent.exited;

      expect(await eventually(() => !groupAlive(reaperGroup), 15_000)).toBe(true);
      expect(groupAlive(held)).toBe(false);
      expect(await Bun.file(signals).exists()).toBe(false);
      expect(groupAlive(group)).toBe(true);
    } finally {
      parent.kill("SIGKILL");
    }
  },
  // The row drives a parent through one foreground step, kills it, and reads the reaper's stops as they end.
  ROW_MS
);

/** A directory holding stand-ins for `docker`, `initdb` and `pg_ctl`. Each appends how it was run
 *  to `calls`, one line per run, with its own pid first. A `pg_ctl start` then hangs, as a start
 *  that has not answered yet does. So does a `docker run`, unless `runFor` is given: it then runs
 *  that many seconds under its own arguments, as a `docker run` its killed starter left behind
 *  goes on to do, makes its container and answers. Docker holds the containers its runs made, a
 *  file of each one's name in `<dir>/containers`: `container inspect` answers whether the name's
 *  file is there, and `rm` deletes it. A making and a removal each add a line of their own to
 *  `calls`, `<pid> made <name>` before the file is there and `<pid> removed <name>` once it is
 *  gone, so a removal of a container is always after its making in `calls`. Every other call
 *  answers at once. */
async function startingStandIns(
  dir: string,
  runFor?: number
): Promise<{ bin: string; calls: string }> {
  const bin = join(dir, "bin");
  const calls = join(dir, "calls");
  const containers = join(dir, "containers");
  const record = `printf '%s %s %s\\n' "$$" "$(basename "$0")" "$*" >> ${JSON.stringify(calls)}`;
  const held = `${JSON.stringify(containers)}/"$name"`;
  const run =
    runFor === undefined
      ? "exec sleep 600"
      : `sleep ${runFor}
    name= previous=
    for word in "$@"; do [ "$previous" = --name ] && name=$word; previous=$word; done
    note made
    touch ${held}
    echo stand-in-id`;
  const scripts: Record<string, string> = {
    docker: `${record}
note() { printf '%s %s %s\\n' "$$" "$1" "$name" >> ${JSON.stringify(calls)}; }
case "$1" in
  run)
    ${run} ;;
  container) name=$3; [ "$2" = inspect ] && { [ -e ${held} ]; exit; } ;;
  rm)
    name=\${!#}
    if [ -e ${held} ]; then rm -f -- ${held}; note removed; fi ;;
esac
`,
    initdb: `${record}\n`,
    pg_ctl: `${record}\nfor word in "$@"; do if [ "$word" = start ]; then exec sleep 600; fi; done\n`,
  };
  await Bun.spawn(["mkdir", "-p", bin, containers]).exited;
  for (const [name, body] of Object.entries(scripts)) {
    await Bun.write(join(bin, name), `#!/usr/bin/env bash\n${body}`);
    await Bun.spawn(["chmod", "+x", join(bin, name)]).exited;
  }
  return { bin, calls };
}

/** The lines `calls` holds, each `<pid> <command> <arguments>`. */
async function callsIn(calls: string): Promise<string[]> {
  return (
    await Bun.file(calls)
      .text()
      .catch(() => "")
  )
    .split("\n")
    .filter((line) => line !== "");
}

/** Kills each stand-in still sleeping in a start, so a row leaks nothing itself. */
async function endHangingStarts(calls: string): Promise<void> {
  for (const line of await callsIn(calls)) {
    try {
      process.kill(Number(line.split(" ")[0]), "SIGKILL");
    } catch {
      // ESRCH: it is gone.
    }
  }
}

/** Starts one stack from `provider`'s variables in a parent process of its own, which hangs in
 *  its start, waits for the start call `started` matches, kills the parent with SIGKILL, and
 *  answers every call the stand-ins took once `settled` holds of them (by default, once one more
 *  call has come after the kill). `runFor` is the stand-in `docker run`'s, as `startingStandIns`
 *  reads it. */
async function killedWhileStarting(
  dir: string,
  provider: Record<string, string>,
  started: RegExp,
  options: { runFor?: number; settled?: (answered: string[]) => boolean } = {}
): Promise<string[]> {
  const { bin, calls } = await startingStandIns(dir, options.runFor);
  const answered = async (): Promise<string[]> =>
    (await callsIn(calls)).map((line) => line.slice(line.indexOf(" ") + 1));
  const parent = Bun.spawn(
    [
      "bun",
      "-e",
      `import { stackPostgres } from ${JSON.stringify(harnessPostgres)};
       const provider = stackPostgres({ dir: ${JSON.stringify(dir)}, run: "starting" });
       await provider.start(0);`,
    ],
    {
      cwd: dispatchPackage,
      env: {
        ...process.env,
        [pgBinVariable]: undefined,
        [pgConnectionsVariable]: "20",
        [pgImageVariable]: undefined,
        PATH: `${bin}:${process.env.PATH ?? ""}`,
        ...provider,
      },
      stderr: "inherit",
      stdout: "ignore",
    }
  );
  try {
    const reached = await eventually(
      async () => (await callsIn(calls)).some((line) => started.test(line)),
      ROW_MS / 2
    );
    if (!reached) throw new Error(`the parent never began its start: ${await callsIn(calls)}`);
    parent.kill("SIGKILL");
    await parent.exited;
    // The reaper's own calls land after the kill; nothing raises an event for them here.
    const before = (await callsIn(calls)).length;
    const settled = options.settled ?? ((lines: string[]) => lines.length > before);
    await eventually(async () => settled(await answered()), 15_000);
    return await answered();
  } finally {
    parent.kill("SIGKILL");
    await endHangingStarts(calls);
  }
}

// The reaper must know a stack's container before `docker run` answers, or a parent killed while
// the image starts leaves a container no one will remove: its id does not exist until that
// answer. So the harness names each container itself, uniquely to the run, hands that name to
// the reaper first, and only then runs it. A `docker` whose `run` never answers stands in for a
// start the kill interrupts.
test(
  "a stack's container whose parent is killed while it starts is removed by the name it was given",
  async () => {
    const answered = await killedWhileStarting(
      makeDirectory(),
      { [pgImageVariable]: "stand-in-image" },
      / docker run /u
    );
    const name = /--name (\S+)/u.exec(
      answered.find((call) => call.startsWith("docker run")) ?? ""
    )?.[1];
    expect(name).toMatch(/^dispatch-e2e-pg-starting-[0-9a-f]{8}-0-1$/u);
    expect(answered).toContain(`docker rm --force --volumes ${name}`);
  },
  // The row drives a parent into a stand-in docker start, kills it, and waits for the reaper's one removal.
  ROW_MS
);

// The same for the binaries provider: a cluster's data directory is known before `pg_ctl start`
// runs, and the reaper is told it first.
test(
  "a cluster whose parent is killed while it starts is stopped by its data directory",
  async () => {
    const dir = makeDirectory();
    const answered = await killedWhileStarting(
      dir,
      { [pgBinVariable]: join(dir, "bin") },
      / pg_ctl .* start$/u
    );
    expect(answered).toContain(`pg_ctl -D ${join(dir, "template", "pgdata")} -m immediate -w stop`);
  },
  // The row drives a parent into a stand-in pg_ctl start, kills it, and waits for the reaper's one stop.
  ROW_MS
);

// A `docker run` its killed starter left behind goes on, and can make the container after the
// reaper's first removal. So the reaper removes the name again while a process of that name still
// runs. The stand-in's `run` goes on for three seconds under its own arguments after the kill, then
// makes its container, and the row waits for that container's removal: a reaper that removes the
// name only before anything made it leaves the container the run makes after.
test(
  "a container a killed parent's docker run makes after the reaper's first removal is removed too",
  async () => {
    const dir = makeDirectory();
    const answered = await killedWhileStarting(
      dir,
      { [pgImageVariable]: "stand-in-image" },
      / docker run /u,
      { runFor: 3, settled: (lines) => lines.some((line) => line.startsWith("removed ")) }
    );
    const name = /--name (\S+)/u.exec(
      answered.find((call) => call.startsWith("docker run")) ?? ""
    )?.[1];
    expect(name).toMatch(/^dispatch-e2e-pg-starting-[0-9a-f]{8}-0-1$/u);
    // The run made its container after the reaper's first removal of the name, and the reaper
    // removed that container after it was made: Docker holds it no longer.
    expect(answered).toContain(`made ${name}`);
    const made = answered.indexOf(`made ${name}`);
    expect(answered.slice(0, made)).toContain(`docker rm --force --volumes ${name}`);
    expect(answered.slice(made)).toContain(`removed ${name}`);
    expect(await Bun.file(join(dir, "containers", String(name))).exists()).toBe(false);
    expect(
      answered.filter((call) => call.startsWith("docker rm --force --volumes ")).length
    ).toBeGreaterThanOrEqual(2);
  },
  // The row drives a killed parent's stand-in docker run past the reaper's first removal and its
  // making of the container, and waits for that container's removal.
  ROW_MS
);

/** Whether anything accepts a connection on the unix socket at `path`: a server that is gone
 *  leaves none there, or leaves its socket file with no listener, and either refuses. */
function serving(path: string): Promise<boolean> {
  const { promise, resolve } = Promise.withResolvers<boolean>();
  const socket = connect({ path });
  socket.once("connect", () => {
    socket.destroy();
    resolve(true);
  });
  socket.once("error", () => resolve(false));
  return promise;
}

// A stack's Postgres needs the provider a run of the browser suite names; without one this row
// would prove nothing and is skipped, saying so, as e2e/__tests__/harness-stacks.test.ts skips the
// case that needs a database.
const provider =
  (process.env[pgImageVariable] ?? "").trim() !== "" ||
  (process.env[pgBinVariable] ?? "").trim() !== "";
if (!provider) {
  console.warn(
    `Skipped the test in ${import.meta.path} that needs a stack's Postgres. This run names ` +
      `neither ${pgImageVariable} nor ${pgBinVariable}. Name one and run the tests again.`
  );
}

test.skipIf(!provider)(
  "a stack's Postgres whose parent is killed serves no longer",
  async () => {
    const dir = makeDirectory();
    const socketFile = join(dir, "socket-directory");
    // The connection limit is given rather than read through `go`, which this row has no need of.
    const env = { ...process.env, [pgConnectionsVariable]: "20" };
    const parent = Bun.spawn(
      [
        "bun",
        "-e",
        `import { writeFileSync } from "node:fs";
         import { stackPostgres } from ${JSON.stringify(harnessPostgres)};
         const provider = stackPostgres({ dir: ${JSON.stringify(dir)}, run: "orphans-" + process.pid });
         const stack = await provider.start(0);
         writeFileSync(${JSON.stringify(socketFile)}, stack.socketDirectory + "\\n");
         setInterval(() => {}, 1 << 30);`,
      ],
      { cwd: dispatchPackage, env, stderr: "inherit", stdout: "ignore" }
    );
    try {
      const written = await eventually(
        async () =>
          /\n$/u.test(
            await Bun.file(socketFile)
              .text()
              .catch(() => "")
          ),
        POSTGRES_ROW_MS / 2
      );
      if (!written) throw new Error("the parent started no Postgres: it wrote no socket directory");
      // The socket file the stack's server makes in its socket directory for the port it is told
      // (`socketPort`, e2e/harness-postgres.ts): the server listens on no TCP address.
      const socket = join((await Bun.file(socketFile).text()).trim(), ".s.PGSQL.5432");
      expect(await serving(socket)).toBe(true);

      parent.kill("SIGKILL");
      await parent.exited;

      await eventually(async () => !(await serving(socket)), 30_000);
      expect(await serving(socket)).toBe(false);
    } finally {
      parent.kill("SIGKILL");
      // What a failing row leaves is removed here by the exact name or directory this run's
      // provider gave its one stack (e2e/harness-postgres.ts), so the row leaks nothing itself:
      // a container's name begins with this row's own run, `orphans-<the parent's pid>`.
      const bin = (process.env[pgBinVariable] ?? "").trim();
      if (bin === "") {
        const listed = Bun.spawn(
          [
            "docker",
            "ps",
            "--all",
            "--filter",
            `name=^dispatch-e2e-pg-orphans-${parent.pid}-`,
            "--format",
            "{{.Names}}",
          ],
          { stderr: "ignore", stdout: "pipe" }
        );
        for (const name of (await new Response(listed.stdout).text()).split("\n")) {
          if (name === "") continue;
          await Bun.spawn(["docker", "rm", "--force", "--volumes", name], {
            stderr: "ignore",
            stdout: "ignore",
          }).exited;
        }
      } else {
        await Bun.spawn(
          [join(bin, "pg_ctl"), "-D", join(dir, "worker-0", "pgdata"), "-m", "immediate", "stop"],
          { stderr: "ignore", stdout: "ignore" }
        ).exited;
      }
    }
  },
  // The row drives a real Postgres container's first start, runs its initdb, kills the parent, and waits for the removal.
  POSTGRES_ROW_MS
);
