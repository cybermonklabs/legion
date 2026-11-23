/** What the binaries provider of e2e/harness-postgres.ts does when the postmaster it started ends
 *  while the start waits for it to answer `select 1`, as a server that crashes on its way up does,
 *  and when the template's server will not stop once the template is whole. The wait (`ready`) has
 *  no bound of its own and reads the postmaster's end through `postmasterGone`: a refusal must name
 *  the server that stopped, at once, rather than wait for ever. And a template is copied only once
 *  its server has stopped: a stop that fails must refuse the start by name. Found by Tester4958-2,
 *  whose plants read every postmaster as running, read a data directory with no `postmaster.pid`
 *  as running, and let the whole template's stop fail in silence, and passed every row before these;
 *  this file's provider rows follow that tester's probes.
 *
 *  The first row reads `postmasterGone` itself, against a pid that runs, one that ended, and a data
 *  directory with no `postmaster.pid`, and runs everywhere. The provider rows start the real
 *  binaries provider in a child of their own. Two kill a held server: a stand-in `psql` first on
 *  the child's PATH answers as a server starting up does while a hold file exists and runs the
 *  real psql otherwise; once the held server's postmaster has written its pid and a held `psql`
 *  has been refused, the child SIGKILLs that postmaster and prints what the start answered. The
 *  template's row holds from the start; the stack's sets the hold as the stack's data directory
 *  appears, so the template is made by the real psql first. The third hands the provider a
 *  binaries directory whose `pg_ctl` refuses to stop the template's server and runs the real one
 *  for everything else; the child then stops that server with the real `pg_ctl` before it exits,
 *  and the row requires the server's own log to say it shut down. They need
 *  `DISPATCH_E2E_PG_BIN`, the binaries that make the databases, and a psql on PATH, and are
 *  skipped without them, saying so. A start that never ends is ended by the child's `oneDatabase`
 *  bound, which names it.
 *
 *  Each provider row removes the clusters it made in its own `finally`, within its own limit. The
 *  file's `temporaryDirectory` removes the rest in an `afterAll`, which runs under Bun's 5 s hook
 *  default: a hook that removed several clusters there failed at 6.5 s under 36 busy loops on 6
 *  CPUs (Tester4958-2's run of 2026-11-23). */

import { beforeAll, expect, test } from "bun:test";
import {
  chmodSync,
  existsSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

import { postmasterGone } from "../harness-postgres";
import { rowLimit, runBounded } from "./bounded-subprocess";

const harnessPostgres = fileURLToPath(new URL("../harness-postgres.ts", import.meta.url));
const stubs = temporaryDirectory("harness-postgres-ends-");
const bin = (process.env.DISPATCH_E2E_PG_BIN ?? "").trim();
const realPsql = Bun.which("psql") ?? "";
const withoutBinaries = bin === "" || realPsql === "";
if (withoutBinaries) {
  console.error(
    "e2e/__tests__/harness-postgres-ends.test.ts: the provider rows are skipped, since " +
      (bin === "" ? "DISPATCH_E2E_PG_BIN names no Postgres binaries" : "no psql is on PATH") +
      " to start the template's and a stack's server with."
  );
}
const child = join(stubs, "end-during-start.ts");

beforeAll(() => {
  writeFileSync(
    child,
    `// Starts stack 0 of the binaries provider and prints what the start answered. For the
// \`template\` and \`stack\` targets it SIGKILLs the held server's postmaster once its pid is
// written and a held psql was refused. For \`stopfails\` it kills nothing, and stops the template's
// server with the real pg_ctl (the last argument) once the start has settled, before it exits: the
// bound the row puts on this child would otherwise take that server with SIGKILL.
import { execFileSync } from "node:child_process";
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";

const [module, target, dir, hold, calls, realPgCtl] = process.argv.slice(2);
const { stackPostgres } = await import(module);
const provider = stackPostgres({ dir, run: "ends-" + target });
let settled = false;
const starting = provider.start(0).then(
  async (database) => {
    settled = true;
    await database.stop(false).catch(() => {});
    return { started: true };
  },
  (error) => {
    settled = true;
    return { message: error.message };
  }
);
// Each wait below ends on what it waits for or on the start settling; the bound the row puts on
// this child ends one that never does.
const until = async (fact) => {
  while (!fact()) await Bun.sleep(5);
};
const data = join(dir, target === "template" ? "template" : "worker-0", "pgdata");
let killed = null;
if (target !== "stopfails") {
  if (target === "stack") {
    await until(() => existsSync(data) || settled);
    writeFileSync(hold, "");
  }
  const pidFile = join(data, "postmaster.pid");
  await until(() => (existsSync(pidFile) && existsSync(calls)) || settled);
  if (!settled) {
    killed = Number(readFileSync(pidFile, "utf8").split("\\n")[0]);
    process.kill(killed, "SIGKILL");
  }
}
const answer = await starting;
if (target === "stopfails" && realPgCtl !== undefined && realPgCtl !== "") {
  const template = join(dir, "template", "pgdata");
  if (existsSync(join(template, "postmaster.pid"))) {
    try {
      execFileSync(realPgCtl, ["-D", template, "-m", "immediate", "-w", "stop"], { stdio: "pipe" });
    } catch (error) {
      console.error("the child's stop of the template's server failed: " + error.message);
    }
  }
}
console.log(JSON.stringify({ killed, ...answer }));
`
  );
});

test(
  "postmasterGone reads a postmaster that runs as running, and one that ended or wrote no pid as gone",
  async () => {
    const data = join(stubs, "gone-data");
    mkdirSync(data);
    // This process runs; the shell below has ended, and been reaped, by the time it is read.
    writeFileSync(join(data, "postmaster.pid"), `${process.pid}\n${data}\n`);
    expect(postmasterGone(data)(), "this running process read as gone").toBe(false);
    const ended = await runBounded({
      argv: ["/bin/sh", "-c", "echo $$"],
      bound: "short",
      from: import.meta.url,
    });
    const pid = Number(ended.stdout.trim());
    expect(Number.isInteger(pid) && pid > 0, `${ended.named} printed ${ended.stdout}`).toBe(true);
    writeFileSync(join(data, "postmaster.pid"), `${pid}\n${data}\n`);
    expect(postmasterGone(data)(), `pid ${pid}, which has ended, read as running`).toBe(true);
    // A data directory with no postmaster.pid: the server pg_ctl started has gone, and a wait
    // that read it as running would wait for ever.
    const empty = join(stubs, "no-pid-data");
    mkdirSync(empty);
    expect(postmasterGone(empty)(), "a data directory with no postmaster.pid read as running").toBe(
      true
    );
  },
  rowLimit("short")
);

for (const row of [
  { target: "template", what: "the stacks' Postgres template" },
  { target: "stack", what: "stack 0's Postgres" },
] as const) {
  test.skipIf(withoutBinaries)(
    `${row.what}, its postmaster killed while the start waits, is refused at once by name`,
    async () => {
      const dir = join(stubs, row.target);
      const shims = join(stubs, `${row.target}-shims`);
      const hold = join(stubs, `${row.target}-hold`);
      const calls = join(stubs, `${row.target}-held-calls`);
      mkdirSync(dir);
      try {
        mkdirSync(shims);
        writeFileSync(
          join(shims, "psql"),
          `#!/bin/sh
if [ -e "${hold}" ]; then
  echo held >> "${calls}"
  echo 'psql: error: connection to server at "127.0.0.1" failed: FATAL:  the database system is starting up' >&2
  exit 2
fi
exec "${realPsql}" "$@"
`
        );
        chmodSync(join(shims, "psql"), 0o755);
        if (row.target === "template") writeFileSync(hold, "");
        const { code, named, stderr, stdout } = await runBounded({
          argv: [process.execPath, child, harnessPostgres, row.target, dir, hold, calls],
          bound: "oneDatabase",
          env: {
            DISPATCH_E2E_PG_BIN: bin,
            HOME: process.env.HOME ?? "",
            PATH: `${shims}:${process.env.PATH ?? ""}`,
            TMPDIR: stubs,
          },
          from: import.meta.url,
        });
        expect(code, `${named} exited ${code}:\n${stderr}`).toBe(0);
        const read = JSON.parse(stdout.trim().split("\n").at(-1) ?? "{}") as {
          killed?: number | null;
          message?: string;
          started?: true;
        };
        expect(read.killed, `${named} printed ${stdout}:\n${stderr}`).toBeGreaterThan(0);
        expect(read.message, `${named} printed ${stdout}:\n${stderr}`).toStartWith(
          `The Dispatch e2e harness cannot start: ${row.what} stopped before it answered select 1: `
        );
      } finally {
        // The bounded child's end has taken every server it started; its clusters go with the row.
        rmSync(dir, { force: true, recursive: true });
      }
    },
    rowLimit("oneDatabase")
  );
}

// The template is copied only once its server has stopped. A `pg_ctl` that refuses that stop -
// exit 1, the server left running - must refuse the start by name rather than let a stack start
// from a copy of a running server. That refusal leaves the template's real server running, and on
// Linux the bounded child's own kill would take it with SIGKILL, which tells nothing of a clean
// stop. So the child stops it with the real `pg_ctl` before it exits, and the row requires the
// server's own log to end its last run with "database system is shut down", which PostgreSQL
// writes on a stop and not on a SIGKILL, and `pg_ctl status` to find no server. The row's own
// `finally` stops a server still running, for a child that never reached its stop.
test.skipIf(withoutBinaries)(
  "the stacks' Postgres template whose server will not stop refuses the start by name",
  async () => {
    const dir = join(stubs, "stopfails");
    const binaries = join(stubs, "stopfails-bin");
    const refused = join(stubs, "stopfails-refused");
    mkdirSync(dir);
    try {
      mkdirSync(binaries);
      for (const tool of readdirSync(bin)) {
        if (tool !== "pg_ctl") symlinkSync(join(bin, tool), join(binaries, tool));
      }
      writeFileSync(
        join(binaries, "pg_ctl"),
        `#!/bin/sh
case " $* " in
  *" stop "*)
    case " $* " in
      *"/template/pgdata "*)
        echo refused >> "${refused}"
        echo "stand-in pg_ctl: this stop is refused" >&2
        exit 1
        ;;
    esac
    ;;
esac
exec "${join(bin, "pg_ctl")}" "$@"
`
      );
      chmodSync(join(binaries, "pg_ctl"), 0o755);
      const templateData = join(dir, "template", "pgdata");
      try {
        const { code, named, stderr, stdout } = await runBounded({
          argv: [
            process.execPath,
            child,
            harnessPostgres,
            "stopfails",
            dir,
            "",
            "",
            join(bin, "pg_ctl"),
          ],
          bound: "oneDatabase",
          env: {
            DISPATCH_E2E_PG_BIN: binaries,
            HOME: process.env.HOME ?? "",
            PATH: process.env.PATH ?? "",
            TMPDIR: stubs,
          },
          from: import.meta.url,
        });
        expect(code, `${named} exited ${code}:\n${stderr}`).toBe(0);
        expect(
          existsSync(refused),
          `the stand-in pg_ctl was never asked to stop the template:\n${stdout}`
        ).toBe(true);
        const read = JSON.parse(stdout.trim().split("\n").at(-1) ?? "{}") as {
          message?: string;
          started?: true;
        };
        expect(read.message, `${named} printed ${stdout}:\n${stderr}`).toStartWith(
          "The Dispatch e2e harness cannot start: the stacks' Postgres template's server would " +
            "not stop, and no stack starts from a copy of a running server: "
        );
        // The server's last run, from its last start, ended in a stop.
        const log = readFileSync(join(dir, "template", "postgres.log"), "utf8");
        const lastRun = log.slice(
          log.lastIndexOf("database system is ready to accept connections")
        );
        expect(
          lastRun,
          `the template's server was not stopped cleanly; its log after its last start:\n${lastRun}`
        ).toContain("database system is shut down");
      } finally {
        // No postmaster.pid: no server is running. A file that is left names a server the child
        // never stopped, or one the bounded child's kill has already taken, whose stop then fails;
        // the checks above and below, not this stop's exit, are the row's verdict.
        if (existsSync(join(templateData, "postmaster.pid"))) {
          await runBounded({
            argv: [join(bin, "pg_ctl"), "-D", templateData, "-m", "immediate", "-w", "stop"],
            bound: "short",
            from: import.meta.url,
          });
        }
      }
      // `pg_ctl status` exits 3 where no server runs on that data directory.
      const status = await runBounded({
        argv: [join(bin, "pg_ctl"), "-D", templateData, "status"],
        bound: "short",
        from: import.meta.url,
      });
      expect(status.code, `the template's server outlived the row:\n${status.stdout}`).toBe(3);
    } finally {
      rmSync(dir, { force: true, recursive: true });
    }
  },
  rowLimit("oneDatabase", "short", "short")
);
