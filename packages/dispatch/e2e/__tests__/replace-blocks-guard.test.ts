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
 *  nothing, and every refusal row asserts the marker is absent. LEGION_TEST_PG_DSN passes through
 *  to the driver where it is set, so the guard compares with the shared database in a pod.
 *
 *  The rows that need a server skip where LEGION_TEST_PG_DSN names none, as in CI's `dispatch`
 *  job; the two of them the guard lets through run on a scratch database they make and drop on
 *  that server, as psql-session.test.ts does. The refusal rows that need none hold strings psql
 *  gets no answer through: a database named `mine`, which the pod's server does not hold, a port
 *  nothing listens on, or a string libpq refuses outright. One more row reads two source files and
 *  runs nothing.
 */

import { expect, test } from "bun:test";
import { randomUUID } from "node:crypto";
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { hostTool } from "@legion/test-fixtures/host-tools";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

import { sqlAt } from "../psql";

const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));
const driverPath = join(dispatchPackage, "e2e/acceptance/replace-blocks.sh");
const localInstancePath = join(dispatchPackage, "e2e/local-instance.ts");

/** One spawn of the driver, which refuses or fails before it starts anything. */
const driverTimeout = 30_000;
/** A spawn of the driver between the creation and the drop of a scratch database. */
const scratchTimeout = 60_000;

/** The pod's shared test database, and the server the rows that need one make a database on. An
 *  issue pod names it, a developer names it, and CI's `dispatch` job has no Postgres, so those rows
 *  skip there. */
const server = process.env.LEGION_TEST_PG_DSN ?? "";
/** Everything the rows that need a server need and this host or run does not have, in one list,
 *  so a host missing both reads one line naming both (psql-session.test.ts composes it the same
 *  way). */
const missing = [
  ...(hostTool("psql") === null ? ["psql on this host's PATH"] : []),
  ...(server === ""
    ? [
        "LEGION_TEST_PG_DSN, naming the shared test database on a server this run may create a database on",
      ]
    : []),
];
const live = missing.length === 0;
if (!live) {
  console.warn(
    `Skipped the tests in ${import.meta.path} that need a database server. This run does not ` +
      `have: ${missing.join("; ")}. Supply what is missing and run the tests again.`
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

/** The driver run from the package root with `databaseUrl`, LEGION_TEST_PG_DSN where this run has
 *  it, and no other Dispatch, Envoy or libpq variable, so what it does is the guard's own. A `bun`
 *  of the row's own stands first on PATH: it leaves a marker file, prints `log` as the instance's
 *  output, and exits 1, so a string the guard lets through starts nothing. `started` is whether
 *  that `bun` ran. */
async function drive(
  databaseUrl: string,
  log = ""
): Promise<{ code: number; stdout: string; stderr: string; started: boolean }> {
  const bin = fakeBunDirectories();
  const marker = join(bin, "bun-ran");
  const logFile = join(bin, "instance.log");
  writeFileSync(logFile, log);
  writeFileSync(
    join(bin, "bun"),
    `#!/usr/bin/env bash\n: >'${marker}'\ncat '${logFile}'\nexit 1\n`,
    {
      mode: 0o755,
    }
  );
  const child = Bun.spawn(["bash", "e2e/acceptance/replace-blocks.sh"], {
    cwd: dispatchPackage,
    env: {
      DATABASE_URL: databaseUrl,
      HOME: process.env.HOME ?? "",
      PATH: `${bin}:${process.env.PATH ?? ""}`,
      ...(process.env.TMPDIR === undefined ? {} : { TMPDIR: process.env.TMPDIR }),
      ...(server === "" ? {} : { LEGION_TEST_PG_DSN: server }),
    },
    stderr: "pipe",
    stdin: "ignore",
    stdout: "pipe",
  });
  const [code, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ]);
  return { code, stdout, stderr, started: existsSync(marker) };
}

/** A refusal row: the driver exits 2 and the fake `bun` never ran, and stderr holds none of
 *  `secrets`. */
async function expectRefusal(databaseUrl: string, secrets: readonly string[]): Promise<string> {
  const { code, stderr, started } = await drive(databaseUrl);
  expect(
    { code, printed: secrets.filter((secret) => stderr.includes(secret)), started },
    "The refusal wrote part of the connection string to stderr, or the guard let the string " +
      "through to `bun run dispatch:local`. A refusal names what is wrong and prints no part of " +
      "the connection string."
  ).toEqual({ code: 2, printed: [], started: false });
  return stderr;
}

for (const { shape, databaseUrl, secret } of [
  {
    shape: "the keyword form",
    databaseUrl: "host=127.0.0.1 port=5433 password=sekrit dbname=mine",
    secret: "sekrit",
  },
  {
    shape: "the keyword form with a single-quoted password",
    databaseUrl: "host=127.0.0.1 password='se kr it' dbname=mine",
    secret: "se kr it",
  },
  {
    shape: "a password holding a slash, as a base64 one does",
    databaseUrl: withPassword("abc/def+ghi", "mine"),
    secret: "abc/def+ghi",
  },
  {
    shape: "a password holding a question mark",
    databaseUrl: withPassword("abc?def", "mine"),
    secret: "abc?def",
  },
  {
    shape: "a password holding a raw at sign",
    databaseUrl: withPassword("ab@cd", "?db%6Eame=mine"),
    secret: "ab@cd",
  },
  {
    shape: "a password parameter spelled with a capital letter",
    databaseUrl: "postgres://127.0.0.1:5433/?Password=sekrit&db%6Eame=mine",
    secret: "sekrit",
  },
  {
    shape: "a password parameter whose name escapes a capital letter",
    databaseUrl: "postgres://127.0.0.1:5433/?pass%57ord=sekrit&db%6Eame=mine",
    secret: "sekrit",
  },
  {
    shape: "the keyword form with the keyword in capitals",
    databaseUrl: "host=127.0.0.1 PASSWORD=sekrit dbname=mine",
    secret: "sekrit",
  },
]) {
  test(
    `a refusal writes no password to stderr: ${shape}`,
    async () => {
      await expectRefusal(databaseUrl, [secret]);
    },
    driverTimeout
  );
}

test(
  "a refusal of a string no reader can parse names the reason and none of the string",
  async () => {
    // A person who meant user:password and left the host out: libpq reads `qq` as the port,
    // refuses it as no integer, and connects nowhere, so the guard refuses what it could not read.
    const afterScheme = "zz-unparseable:qq";
    const stderr = await expectRefusal(`postgres://${afterScheme}`, [afterScheme]);
    expect(stderr).toContain(
      "DATABASE_URL could not be read: psql, asking through it which database it reaches, got no answer"
    );
  },
  driverTimeout
);

test.skipIf(!live)(
  "a refusal writes no password to stderr: a password of letters, with a percent escape in its query that reaches the shared database",
  async () => {
    const shared = sharedServer();
    await expectRefusal(withPassword("sekrit", `?db%6Eame=${shared.database}`, shared.authority), [
      "sekrit",
    ]);
  },
  driverTimeout
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
    const stderr = await expectRefusal(url, [hidden, "a=b", "dbname=mine"]);
    // Whether the server takes the made-up password decides which refusal is right. A server
    // that trusts the loopback, as this pod's does, answers through the string, so the run would
    // have reached the shared database and must name it; one that checks passwords answers
    // nothing, so the string reaches no database and the run refuses it as unread. Asked here
    // with psql, as the driver asks, rather than assumed.
    const asked = Bun.spawn(["psql", "-XAtqw", `--dbname=${url}`, "-c", "select 1"], {
      stderr: "ignore",
      stdout: "pipe",
    });
    const answered = (await asked.exited) === 0;
    expect(stderr, "The refusal does not give the reason the server's answer calls for.").toContain(
      answered
        ? `DATABASE_URL reaches a database named ${decodeURIComponent(shared.database)}, as LEGION_TEST_PG_DSN does`
        : "DATABASE_URL could not be read"
    );
  },
  driverTimeout
);

test.skipIf(!live)(
  "a scratch database on the shared server is let through to the start",
  async () => {
    await withScratchDatabase(async (scratch) => {
      const { code, stdout, started } = await drive(scratch.url);
      expect(
        {
          code,
          starting: stdout.includes(
            `starting a Dispatch of its own with bun run dispatch:local. It builds the dashboard, builds the server and seeds ${scratch.name} first`
          ),
          started,
        },
        "The guard refused a database of the run's own on the shared server, or the start line " +
          "does not name the database the server answered."
      ).toEqual({ code: 1, starting: true, started: true });
    });
  },
  scratchTimeout
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
      const tail = Bun.spawn(["tail", "-n", "10"], { stdin: new Blob([log]), stdout: "pipe" });
      expect(
        (await new Response(tail.stdout).text()).includes(scratch.url),
        "The fixture log's last ten lines do not hold DATABASE_URL, so this row cannot see a leak."
      ).toBe(true);

      const { code, stderr, started } = await drive(scratch.url, log);
      expect(
        {
          code,
          started,
          stopped: stderr.includes("it stopped while it started"),
          printedUrl: stderr.includes(scratch.url),
          printedHandOver: handOver.filter((line) => stderr.includes(line.trim())),
        },
        "The failed start printed the instance's hand-over message, which holds DATABASE_URL " +
          "whole. Print the log only up to that message."
      ).toEqual({ code: 1, started: true, stopped: true, printedUrl: false, printedHandOver: [] });
    });
  },
  scratchTimeout
);
