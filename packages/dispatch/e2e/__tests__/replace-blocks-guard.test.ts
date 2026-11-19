/** What `e2e/acceptance/replace-blocks.sh` does with the DATABASE_URL it is given before it starts
 *  anything: its guard.
 *
 *  The driver's `bun run dispatch:local` truncates and reseeds the database it is given, so the
 *  guard refuses a DATABASE_URL that reaches the pod's shared test database, the one
 *  LEGION_TEST_PG_DSN names. It parses neither string: it asks the server each string reaches for
 *  `current_database()` and `inet_server_port()` through psql, refuses a string it gets no answer
 *  through, and refuses a DATABASE_URL whose database has the shared database's name unless both
 *  servers answered a port and the ports differ. A refusal exits 2 and names the variable and the
 *  reason, and prints no part of either connection string, since stderr reaches whatever log
 *  keeps it.
 *
 *  Every row that runs the driver puts a `bun` of its own first on PATH, which leaves a marker
 *  file and exits 1, so a guard that lets a string through starts no real Dispatch and truncates
 *  nothing, and every refusal row asserts the marker is absent and the reason the guard gave.
 *  The reason is what makes a refusal the guard's: the driver refuses before its guard, with the
 *  same exit 2 and nothing started, when a command it requires is not on PATH
 *  (require_commands). LEGION_TEST_PG_DSN passes through to the driver where it is set, so the
 *  guard compares with the shared database in a pod.
 *
 *  Every row that runs the driver skips where psql is not on PATH: the driver requires it before
 *  its guard and the guard asks each server through it. The rows that need a server also skip
 *  where LEGION_TEST_PG_DSN names none, as in CI's `dispatch-ui` job; the two of them the guard
 *  lets through run on a scratch database they make and drop on that server, as
 *  psql-session.test.ts does. The refusal rows that need none hold strings psql gets no answer
 *  through: a database named `mine`, which the pod's server does not hold, a port nothing listens
 *  on, or a string libpq refuses outright. One more row reads two source files and runs nothing.
 */

import { expect, test } from "bun:test";
import { randomUUID } from "node:crypto";
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { hostTool } from "@legion/test-fixtures/host-tools";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

import { sqlAt } from "../psql";
import { masked, rowLimit, runBounded } from "./bounded-subprocess";

const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));
const driverPath = join(dispatchPackage, "e2e/acceptance/replace-blocks.sh");
const localInstancePath = join(dispatchPackage, "e2e/local-instance.ts");

/** LEGION_TEST_PG_DSN as this run has it, quotes and all, which `drive` hands the driver
 *  unchanged, so the guard reads it through the driver's own strip of its quotes. */
const exportedServer = process.env.LEGION_TEST_PG_DSN ?? "";
/** The pod's shared test database, and the server the rows that need one make a database on. An
 *  issue pod names it, a developer names it, and CI's `dispatch-ui` job has no Postgres, so those
 *  rows skip there. It is LEGION_TEST_PG_DSN with one `"` or `'` taken off its start and one off
 *  its end, each whether or not the other was there. That is the same rule as the driver's own
 *  strip of it, `${shared_dsn#[\"\']}` then `${shared_dsn%[\"\']}` in
 *  e2e/acceptance/replace-blocks.sh, before its guard reads it, so every row here reads the string
 *  the driver reads. */
const server = exportedServer.replace(/^["']/, "").replace(/["']$/, "");
/** Whether psql is missing from this host's PATH. The driver's DATABASE_URL branch requires it
 *  before its guard runs, so without it every row that runs the driver would see that refusal and
 *  not the guard's: they skip. */
const withoutPsql = hostTool("psql") === null;
/** Everything the rows that need a server need and this host or run does not have, in one list,
 *  so a host missing both reads one line naming both (psql-session.test.ts composes it the same
 *  way). */
const missing = [
  ...(withoutPsql ? ["psql on this host's PATH"] : []),
  ...(server === ""
    ? [
        "LEGION_TEST_PG_DSN, naming the shared test database on a server this run may create a database on",
      ]
    : []),
];
const live = missing.length === 0;
if (!live) {
  console.warn(
    `Skipped the tests in ${import.meta.path} that ` +
      `${withoutPsql ? "run the driver" : "need a database server"}. This run does not have: ` +
      `${missing.join("; ")}. Supply what is missing and run the tests again.`
  );
}

/** The first line of the hand-over message e2e/local-instance.ts prints once the instance is up,
 *  the message that holds DATABASE_URL whole. The fixture log below is built from it, and a row
 *  ties it to the line the instance writes and to the line the driver's cut names. */
const handOverHeading = "Dispatch local instance is up.";

/** The user and the host of a postgres:// URL, which `withPassword` puts around a password. */
type Authority = { user: string; host: string };

/** The authority every row that needs no server writes: nothing answers for the database `mine`
 *  there, in this pod or in CI. */
const localAuthority: Authority = { user: "legion_test", host: "127.0.0.1:5433" };

/** The shared server's user and host, and its database's name as the URL spells it, read from
 *  LEGION_TEST_PG_DSN. Only the rows that need a server call it. */
function sharedServer(): { authority: Authority; database: string } {
  const url = new URL(server);
  return { authority: { user: url.username, host: url.host }, database: url.pathname.slice(1) };
}

/** A postgres:// URL whose userinfo carries `password`. The URL is built here rather than written
 *  whole, so the source holds no `user:password@host` literal for a credential scanner to stop on:
 *  every password in this file is made up for the row that names it. */
function withPassword(password: string, rest: string, authority = localAuthority): string {
  return `postgres://${authority.user}:${password}@${authority.host}/${rest}`;
}

/** A database on the shared server for one row alone, made by `create database` and dropped with
 *  FORCE, and the URL that reaches it: LEGION_TEST_PG_DSN with only its database changed. */
async function withScratchDatabase(
  use: (scratch: { name: string; url: string }) => Promise<void>
): Promise<void> {
  const name = `replace_blocks_guard_${randomUUID().replaceAll("-", "_")}`;
  const url = new URL(server);
  url.pathname = `/${name}`;
  await sqlAt(server, [`create database ${name}`]);
  try {
    await use({ name, url: url.href });
  } finally {
    await sqlAt(server, [`drop database if exists ${name} with (force)`]);
  }
}

const fakeBunDirectories = temporaryDirectories("replace-blocks-fake-bun");
/** A directory for the fixture log the failed-start row hands `tail`, one per row. */
const logDirectories = temporaryDirectories("replace-blocks-log");

/** The driver run from the package root with `databaseUrl`, LEGION_TEST_PG_DSN as this run has it
 *  where it has one, `label` as DISPATCH_E2E_CASE, and no other Dispatch, Envoy or libpq variable,
 *  so what it does is the guard's own. The driver never reads DISPATCH_E2E_CASE, and its scrub
 *  unsets it with every other DISPATCH_ variable: the runs differ only in DATABASE_URL, which the
 *  cost line names without its value, so `label`, one per case, is what tells their cost lines
 *  apart. A `bun` of the row's own stands first on PATH: it leaves a marker file, prints `log` as
 *  the instance's output, and exits 1, so a string the guard lets through starts nothing.
 *  `started` is whether that `bun` ran. A `ps` of the row's own stands beside it: it leaves a
 *  marker file and answers nothing, so `psRan` is whether the driver started one. The start reads
 *  each process group through proc_group (scripts/e2e/lib/process-tree.sh), from the one
 *  process's /proc/<pid>/stat: procps's `ps -o pgid= -p PID` reads every process of the host for
 *  that answer, 2.17 s a call on a devbox at load 475, and the two a row's start made went past
 *  the driver's short bound when each cost 31 s. */
async function drive(
  label: string,
  databaseUrl: string,
  log = ""
): Promise<{ code: number; stdout: string; stderr: string; started: boolean; psRan: boolean }> {
  const bin = fakeBunDirectories();
  const marker = join(bin, "bun-ran");
  const psMarker = join(bin, "ps-ran");
  const logFile = join(bin, "instance.log");
  writeFileSync(logFile, log);
  writeFileSync(
    join(bin, "bun"),
    `#!/usr/bin/env bash\n: >'${marker}'\ncat '${logFile}'\nexit 1\n`,
    {
      mode: 0o755,
    }
  );
  writeFileSync(join(bin, "ps"), `#!/usr/bin/env bash\n: >'${psMarker}'\nexit 1\n`, {
    mode: 0o755,
  });
  const { code, stdout, stderr } = await runBounded({
    argv: ["bash", "e2e/acceptance/replace-blocks.sh"],
    bound: "short",
    cwd: dispatchPackage,
    env: {
      DATABASE_URL: databaseUrl,
      DISPATCH_E2E_CASE: label,
      HOME: process.env.HOME ?? "",
      PATH: `${bin}:${process.env.PATH ?? ""}`,
      ...(process.env.TMPDIR === undefined ? {} : { TMPDIR: process.env.TMPDIR }),
      ...(exportedServer === "" ? {} : { LEGION_TEST_PG_DSN: exportedServer }),
    },
    from: import.meta.url,
  });
  return { code, stdout, stderr, started: existsSync(marker), psRan: existsSync(psMarker) };
}

/** What a row's failure says of a driver that ended, whatever its exit code: that code and its
 *  whole stderr. A driver its short bound stopped never reaches here: `runBounded` throws before
 *  `drive` returns, and the row fails with that message, which names the child, the bound and the
 *  time waited. Each refusal of the driver's writes its reason on its stderr, naming the variable
 *  or the command it refused, so the failure says which refusal fired, or that none did. The
 *  stderr is read through `masked`, as a cost line's command is: a run that wrote a connection
 *  string there would otherwise carry its password into the failure, and the run's output reaches
 *  whatever log keeps it. */
function driverSaid({ code, stderr }: { code: number; stderr: string }): string {
  return ` The driver exited ${code}, and its stderr read:\n${masked(stderr) || "(nothing)"}`;
}

/** The reason the driver's `unread_dsn` gives for a DATABASE_URL psql got no answer through. */
const unreadReason =
  "DATABASE_URL could not be read: psql, asking through it which database it reaches, got no answer this run can read.";

/** The reason the driver's `shared_database` gives for a DATABASE_URL that reaches a database
 *  named `database`, as LEGION_TEST_PG_DSN does. */
function sharedReason(database: string): string {
  return `DATABASE_URL reaches a database named ${database}, as LEGION_TEST_PG_DSN does`;
}

/** The reason the guard owes `url`, a string for the shared database on the shared server that
 *  carries a made-up password. Whether the server takes that password decides it. A server that
 *  trusts the loopback, as this pod's does, answers through the string, so the run would have
 *  reached the shared database and must name it; one that checks passwords answers nothing, so
 *  the string reaches no database and the run refuses it as unread. Asked here with psql and the
 *  flags the driver's read gives it, rather than assumed. */
async function sharedDatabaseRefusal(url: string): Promise<string> {
  const asked = await runBounded({
    argv: ["psql", "-XAtqw", `--dbname=${url}`, "-c", "select 1"],
    bound: "short",
    from: import.meta.url,
  });
  return asked.code === 0
    ? sharedReason(decodeURIComponent(sharedServer().database))
    : unreadReason;
}

/** A refusal row: the driver, run under `label` (`drive`), exits 2, the fake `bun` never ran,
 *  stderr gives `reason`, one of the guard's own two (unreadReason, sharedReason), and stderr holds
 *  none of `secrets`. The reason is what makes the refusal the guard's: the driver's
 *  require_commands also exits 2 and starts nothing when a command the driver needs is not on
 *  PATH, and a row that asserted the rest alone would pass without the guard running. Where stderr
 *  gives another reason, the row's failure shows the first line of stderr in its place, and its
 *  message the exit code and the whole stderr (`driverSaid`). */
async function expectRefusal(
  label: string,
  databaseUrl: string,
  reason: string,
  secrets: readonly string[]
): Promise<void> {
  const run = await drive(label, databaseUrl);
  const { code, stderr, started } = run;
  expect(
    {
      code,
      reason: stderr.includes(reason) ? reason : stderr.split("\n", 1)[0],
      printed: secrets.filter((secret) => stderr.includes(secret)),
      started,
    },
    "The refusal is not the guard's, wrote part of the connection string to stderr, or the " +
      "guard let the string through to `bun run dispatch:local`. A refusal of the guard's names " +
      "what is wrong and prints no part of the connection string." +
      driverSaid(run)
  ).toEqual({ code: 2, reason, printed: [], started: false });
}

for (const { label, shape, databaseUrl, secret } of [
  {
    label: "keyword",
    shape: "the keyword form",
    databaseUrl: "host=127.0.0.1 port=5433 password=sekrit dbname=mine",
    secret: "sekrit",
  },
  {
    label: "keyword-quoted",
    shape: "the keyword form with a single-quoted password",
    databaseUrl: "host=127.0.0.1 password='se kr it' dbname=mine",
    secret: "se kr it",
  },
  {
    label: "url-slash",
    shape: "a password holding a slash, as a base64 one does",
    databaseUrl: withPassword("abc/def+ghi", "mine"),
    secret: "abc/def+ghi",
  },
  {
    label: "url-question-mark",
    shape: "a password holding a question mark",
    databaseUrl: withPassword("abc?def", "mine"),
    secret: "abc?def",
  },
  {
    label: "url-at-sign",
    shape: "a password holding a raw at sign",
    databaseUrl: withPassword("ab@cd", "?db%6Eame=mine"),
    secret: "ab@cd",
  },
  {
    label: "parameter-capital",
    shape: "a password parameter spelled with a capital letter",
    databaseUrl: "postgres://127.0.0.1:5433/?Password=sekrit&db%6Eame=mine",
    secret: "sekrit",
  },
  {
    label: "parameter-escaped-capital",
    shape: "a password parameter whose name escapes a capital letter",
    databaseUrl: "postgres://127.0.0.1:5433/?pass%57ord=sekrit&db%6Eame=mine",
    secret: "sekrit",
  },
  {
    label: "keyword-capitals",
    shape: "the keyword form with the keyword in capitals",
    databaseUrl: "host=127.0.0.1 PASSWORD=sekrit dbname=mine",
    secret: "sekrit",
  },
]) {
  test.skipIf(withoutPsql)(
    `a refusal writes no password to stderr: ${shape}`,
    async () => {
      await expectRefusal(label, databaseUrl, unreadReason, [secret]);
    },
    // The driver, a bounded child under the short bound.
    rowLimit("short")
  );
}

test.skipIf(withoutPsql)(
  "a refusal of a string no reader can parse names the reason and none of the string",
  async () => {
    // A person who meant user:password and left the host out: libpq reads `qq` as the port,
    // refuses it as no integer, and connects nowhere, so the guard refuses what it could not read.
    const afterScheme = "zz-unparseable:qq";
    await expectRefusal("unparseable", `postgres://${afterScheme}`, unreadReason, [afterScheme]);
  },
  // The driver, a bounded child under the short bound.
  rowLimit("short")
);

test.skipIf(!live)(
  "a refusal writes no password to stderr: a password of letters, with a percent escape in its query that reaches the shared database",
  async () => {
    const shared = sharedServer();
    const url = withPassword("sekrit", `?db%6Eame=${shared.database}`, shared.authority);
    await expectRefusal("shared-percent-escape", url, await sharedDatabaseRefusal(url), ["sekrit"]);
  },
  // The psql that asks whether the server takes the password, then the driver, each a bounded
  // child under the short bound.
  rowLimit("short", "short")
);

test.skipIf(!live)(
  "a password that hides a dbname= keyword is refused for the shared database the server names",
  async () => {
    // The reviewer's string: libpq reads everything before the `@` as the userinfo, so it
    // connects to the database in the path, the shared one, while a reader of the text finds
    // `dbname=mine` in what looks like a query.
    // Named `hidden`: the public leak check refused this line's first push, written as an
    // assignment to `password`, as a credential, though the value is made up.
    const hidden = "x?a=b&dbname=mine&";
    const shared = sharedServer();
    const url = withPassword(hidden, shared.database, shared.authority);
    await expectRefusal("shared-hidden-dbname", url, await sharedDatabaseRefusal(url), [
      hidden,
      "a=b",
      "dbname=mine",
    ]);
  },
  // The psql that asks whether the server takes the password, then the driver, each a bounded
  // child under the short bound.
  rowLimit("short", "short")
);

test.skipIf(!live)(
  "a scratch database on the shared server is let through to the start, which starts no ps",
  async () => {
    await withScratchDatabase(async (scratch) => {
      const run = await drive("scratch-let-through", scratch.url);
      const { code, stdout, started, psRan } = run;
      expect(
        {
          code,
          starting: stdout.includes(
            `starting a Dispatch of its own with bun run dispatch:local. It builds the dashboard, builds the server and seeds ${scratch.name} first`
          ),
          started,
          psRan,
        },
        "The guard refused a database of the run's own on the shared server, the start line " +
          "does not name the database the server answered, or the start read a process group " +
          "through ps." +
          driverSaid(run)
      ).toEqual({ code: 1, starting: true, started: true, psRan: false });
    });
  },
  // The driver, a bounded child under the short bound. The scratch database's create and drop go
  // through `sqlAt`, the psql edge no bound reaches, inside the 30 000 ms the limit adds for the
  // row's own work.
  rowLimit("short")
);

/** The log a start leaves when the driver's read misses the signed-in line and the instance then
 *  stops: what came before the hand-over, then the hand-over message itself, which
 *  e2e/local-instance.ts writes in one console.log holding DATABASE_URL whole, twice. The
 *  signed-in line is left out of it, which is the read missing it: with that line the driver
 *  takes the run for started and prints no log at all. */
function stoppedInstanceLog(databaseUrl: string): { log: string; handOver: string[] } {
  const handOver = [
    handOverHeading,
    `  Database (truncated and reseeded on every start): ${databaseUrl}`,
    "  Run a spec against it from packages/dispatch (this empties the instance; restart it to reseed):",
    `    DISPATCH_E2E_REUSE_SERVERS=1 DATABASE_URL=${databaseUrl} bunx playwright test`,
    "Press Ctrl-C to stop.",
  ];
  const log = [
    "$ cd packages/dispatch && bun run dispatch:local",
    "$ bun e2e/local-instance.ts",
    "",
    ...handOver,
    "",
  ].join("\n");
  return { log, handOver };
}

test("the hand-over heading is the line the instance writes and the line the driver cuts at", () => {
  const source = readFileSync(localInstancePath, "utf8");
  const written = JSON.stringify(`\n${handOverHeading}\n`);
  expect(
    source.split(written).length - 1,
    `e2e/local-instance.ts does not write ${written} once, as a line of its own. The driver ` +
      "cuts the instance's log at that line, and this file's fixture log is built from it."
  ).toBe(1);
  const cut = `sed '/^${handOverHeading.replaceAll(".", "\\.")}/,$d'`;
  expect(
    readFileSync(driverPath, "utf8"),
    `replace-blocks.sh does not cut the instance's log at ${handOverHeading}: its cut no ` +
      "longer names the line local-instance.ts writes, so a failed start prints the hand-over."
  ).toContain(cut);
});

test.skipIf(!live)(
  "a start that fails prints the instance's log without the hand-over message",
  async () => {
    await withScratchDatabase(async (scratch) => {
      const { log, handOver } = stoppedInstanceLog(scratch.url);
      // The control: the last ten lines of that log, as a plain tail reads them, hold the URL, so
      // a row that finds none of it in stderr has seen the leak it guards against and not a log
      // too short to carry it.
      const logFile = join(logDirectories(), "instance.log");
      writeFileSync(logFile, log);
      const tail = await runBounded({
        argv: ["tail", "-n", "10", logFile],
        bound: "short",
        from: import.meta.url,
      });
      expect(
        tail.stdout.includes(scratch.url),
        "The fixture log's last ten lines do not hold DATABASE_URL, so this row cannot see a leak."
      ).toBe(true);

      const run = await drive("failed-start", scratch.url, log);
      const { code, stderr, started, psRan } = run;
      expect(
        {
          code,
          started,
          stopped: stderr.includes("it stopped while it started"),
          printedUrl: stderr.includes(scratch.url),
          printedHandOver: handOver.filter((line) => stderr.includes(line.trim())),
          psRan,
        },
        "The failed start printed the instance's hand-over message, which holds DATABASE_URL " +
          "whole, or read a process group through ps. Print the log only up to that message, " +
          "and read each group through proc_group." +
          driverSaid(run)
      ).toEqual({
        code: 1,
        started: true,
        stopped: true,
        printedUrl: false,
        printedHandOver: [],
        psRan: false,
      });
    });
  },
  // Two bounded children one after the other, `tail` then the driver, each under the short
  // bound. The scratch database's create and drop go through `sqlAt`, the psql edge no bound
  // reaches, inside the 30 000 ms the limit adds for the row's own work.
  rowLimit("short", "short")
);
