/** What `e2e/acceptance/node-projects.sh` takes for a transaction of its own database holding the
 *  event horizon back, on which its wait for the server's object pass fails at once. The driver
 *  watches the horizon from 5 s before it sends its event to the end of that wait
 *  (`start_horizon_watch`, then `watch_horizon` on each poll): it observes the horizon about once
 *  a second (`observe_horizon`), and `own_horizon_hold` names a transaction that has held one of
 *  those observations back for 5 s (`horizontest.OwnHoldBound`). All four are functions of
 *  `e2e/acceptance/lib.sh`, and each row runs them in a bash that sourced it, against a database
 *  of this file alone.
 *
 *  The rule is horizontest's own (`Warning.heldFor`, docs/testing.md, "A test that waits for the
 *  event feed's first frame"): a hold lasts the shorter of the pending observation's age and the
 *  holder's own. A transaction holds an observation back only while it holds a transaction id
 *  drawn before the observer's, since the horizon's proof waits only on those
 *  (`events/horizon.go`, "Safe horizon"). So a transaction that read for longer than the bound and
 *  drew its first id after an observation does not hold that one back, however old its
 *  xact_start, and is named only once a later observation has waited the bound on it; a
 *  transaction that drew its id before an observation is named only once that observation, too,
 *  has waited the bound.
 *
 *  Each row holds its transaction on this process's `sql` session (`e2e/psql.ts`), pointed at the
 *  file's database. The functions read the hold on the database's own clock (xact_start, now()),
 *  which no fake timer of this process can move, so a row waits on that clock, with
 *  `pg_sleep_until`, until the moment the bound has passed for what it holds, and no longer. The
 *  rows need psql and a server they may make a database on, and skip where LEGION_TEST_PG_DSN
 *  names none, as in CI's `dispatch-ui` job (psql-session.test.ts says the same). The row of
 *  `watch_horizon`'s refusal reaches no database and runs everywhere. */

import { afterAll, beforeAll, expect, test } from "bun:test";
import { randomUUID } from "node:crypto";
import { fileURLToPath } from "node:url";
import { captureEnvironment } from "@legion/test-fixtures/environment";
import { hostTool } from "@legion/test-fixtures/host-tools";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

import { dropDatabase, resetSession, sql, sqlAt } from "../psql";
import { type BoundedRun, rowLimit, runBounded } from "./bounded-subprocess";

const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));

/** The server this run may make a database on, as psql-session.test.ts reads it. */
const server = process.env.LEGION_TEST_PG_DSN ?? "";
const missing = [
  ...(hostTool("psql") === null ? ["psql on this host's PATH"] : []),
  ...(server === ""
    ? ["LEGION_TEST_PG_DSN, naming a server this run may create a database on"]
    : []),
];
const live = missing.length === 0;
if (!live) {
  console.warn(
    `Skipped the tests in ${import.meta.path} that need a database. This run does not have: ` +
      `${missing.join("; ")}. Supply what is missing and run the tests again.`
  );
}

/** The database of this file alone: `own_horizon_hold` reads the holders of the database it is
 *  connected to, so no other run's transaction is among them. */
const database = `node_projects_hold_${randomUUID().replaceAll("-", "_")}`;
const databaseUrl = (): string => {
  const url = new URL(server);
  url.pathname = `/${database}`;
  return url.href;
};

/** The bound `own_horizon_hold` holds a transaction to, `horizontest.OwnHoldBound`, in seconds and
 *  as SQL writes an interval. */
const boundSeconds = 5;
const bound = `${boundSeconds} seconds`;

let restoreNames: (() => void) | undefined;

beforeAll(
  async () => {
    if (!live) return;
    await sqlAt(server, [`create database ${database}`]);
    // `sql` resolves its database from the environment when it starts its psql, as
    // psql-session.test.ts says, so the file's database replaces the caller's here.
    restoreNames = captureEnvironment([
      "DATABASE_URL",
      "PLAYWRIGHT_BASE_URL",
      "PLAYWRIGHT_DATABASE_URL",
    ]);
    process.env.DATABASE_URL = databaseUrl();
    delete process.env.PLAYWRIGHT_BASE_URL;
    delete process.env.PLAYWRIGHT_DATABASE_URL;
  },
  // `sqlAt`'s psql is bounded nowhere (bounded-subprocess.ts says why); the minute is what stops
  // a create that hangs.
  60_000
);

afterAll(
  async () => {
    restoreNames?.();
    // The session the rows held their transactions on is connected to the database dropped
    // below, so it ends here, before the next `sql` call of this process reaches it.
    resetSession();
    if (!live) return;
    await dropDatabase(server, database);
  },
  // `dropDatabase`'s psql is `sqlAt`'s; the minute is what stops a drop that waits forever.
  60_000
);

const works = temporaryDirectories("node-projects-hold");

/** This process's libpq variables. A DSN can leave its password, its TLS files or its user to
 *  them, as `sql` and `sqlAt` read them from this process's environment; the bash below gets
 *  them too, so its psql reaches the server they do. lib.sh's `query` takes PGHOSTADDR, PGSERVICE
 *  and PGSERVICEFILE away itself. */
const libpqVariables = Object.fromEntries(
  Object.entries(process.env).filter(
    (entry): entry is [string, string] => entry[0].startsWith("PG") && entry[1] !== undefined
  )
);

/** Runs `script` in a bash that sourced lib.sh, with the file's database as `database_url`, a
 *  directory of the row's own as `work` and `args` as its positional parameters, and answers how
 *  it ended. A function of lib.sh that fails exits that bash 1 with its FAIL line on stderr. */
function runLib(script: string, ...args: string[]): Promise<BoundedRun> {
  return runBounded({
    argv: [
      "bash",
      "-c",
      `database_url=$1 work=$2 && shift 2 && . e2e/acceptance/lib.sh && ${script}`,
      "lib.sh",
      databaseUrl(),
      works(),
      ...args,
    ],
    bound: "short",
    cwd: dispatchPackage,
    env: { ...libpqVariables, PATH: process.env.PATH ?? "", HOME: process.env.HOME ?? "" },
    from: import.meta.url,
  });
}

/** `runLib`'s run of `script`, which must exit 0, answering what it printed: a function of lib.sh
 *  that fails fails the row. */
async function lib(script: string, ...args: string[]): Promise<string> {
  const { code, stdout, stderr, named } = await runLib(script, ...args);
  expect(code, `${named} exited ${code}: ${stderr}`).toBe(0);
  return stdout;
}

/** An observation as `observe_horizon` answers it, the observer's id and its time separated by a
 *  space, which is one argument of `own_horizon_hold`. */
async function observe(): Promise<string> {
  const observation = await lib('observe_horizon hold && printf %s "$answer"');
  expect(observation, "observe_horizon answers the observer's id and its time").toMatch(
    /^\d+ \d+(\.\d+)?$/
  );
  return observation;
}

/** When `observation` was taken, as a timestamptz expression. */
const takenAt = (observation: string): string => `to_timestamp(${observation.split(" ")[1]})`;

/** What a script prints after `own_horizon_hold`: the age of the observation it judged and the
 *  holder it named, both set by the one statement that judged the hold. */
const printHold = 'printf "%s|%s" "$observation_age" "$answer"';

/** One judgement of `own_horizon_hold`, read from what `printHold` printed: how long the newest
 *  observation that had waited the bound had waited, in seconds on the database's clock, or null
 *  where none had; and the holder named, '' for none. */
function holdOf(printed: string): { age: number | null; holder: string } {
  const bar = printed.indexOf("|");
  expect(bar, `own_horizon_hold printed no observation age: ${JSON.stringify(printed)}`).not.toBe(
    -1
  );
  const age = printed.slice(0, bar);
  expect(age, "the age own_horizon_hold read, in seconds, or nothing").toMatch(/^(\d+(\.\d+)?)?$/);
  return { age: age === "" ? null : Number(age), holder: printed.slice(bar + 1) };
}

/** `own_horizon_hold`'s judgement of `observations`, oldest first, in a bash of its own. */
async function check(...observations: string[]): Promise<{ age: number | null; holder: string }> {
  return holdOf(await lib(`own_horizon_hold hold "$@" && ${printHold}`, ...observations));
}

/** Waits until the database's clock has passed `since`, a timestamptz expression, by the bound,
 *  on a connection of its own. A `since` that reads null waits not at all, and the row's next
 *  check then fails on the state it reads. */
async function untilPastTheBound(since: string): Promise<void> {
  await sqlAt(databaseUrl(), [`select pg_sleep_until((${since}) + interval '${bound}')`]);
}

/** Whether backend `pid` holds a transaction id in a transaction open past the bound, read on a
 *  connection of its own: the condition the driver's check named a holder on before it read any
 *  observation, so a row that expects no holder shows its holder is one that condition saw. */
async function heldPastTheBound(pid: string): Promise<string> {
  return sqlAt(databaseUrl(), [
    `select backend_xid is not null and xact_start < now() - interval '${bound}' ` +
      `from pg_stat_activity where pid = ${pid}`,
  ]);
}

// The observation has waited the bound too when the check reads it, so only the order of the two
// transaction ids can keep the holder unnamed.
test.skipIf(!live)(
  "a transaction open past the bound whose first id comes after the observation does not hold it back",
  async () => {
    const pid = await sql("SELECT pg_backend_pid()");
    await sql("BEGIN", "SELECT 1");
    try {
      await untilPastTheBound(`(select xact_start from pg_stat_activity where pid = ${pid})`);
      const observation = await observe();
      await sql("SELECT pg_current_xact_id()");
      await untilPastTheBound(takenAt(observation));
      expect(await heldPastTheBound(pid)).toBe("t");
      const { age, holder } = await check(observation);
      expect(age).toBeGreaterThanOrEqual(boundSeconds);
      expect(
        holder,
        `backend ${pid} drew its first transaction id after the observation, so no proof of it waits on that id`
      ).toBe("");
    } finally {
      await sql("ROLLBACK");
    }
  },
  rowLimit("short", "short")
);

// The server's tick observes on its own schedule, after the driver's first observation as well as
// before it, and a transaction that draws its id after the first holds back every later one: the
// driver observes on each poll, and the newest observation that has waited the bound judges.
test.skipIf(!live)(
  "a transaction whose first id comes after one observation is named once a later one has waited the bound on it",
  async () => {
    const pid = await sql("SELECT pg_backend_pid()");
    await sql("BEGIN", "SELECT 1");
    try {
      const first = await observe();
      await sql("SELECT pg_current_xact_id()");
      const later = await observe();
      await untilPastTheBound(takenAt(later));
      const { age, holder } = await check(first, later);
      expect(age).toBeGreaterThanOrEqual(boundSeconds);
      expect(
        holder,
        `backend ${pid} drew its id before the later observation, which has waited ${age} s on it`
      ).toStartWith(`backend ${pid} (application `);
    } finally {
      await sql("ROLLBACK");
    }
  },
  rowLimit("short", "short", "short")
);

/** `observation`, its time written as the clock of the statement that judges it, less `seconds`.
 *  own_horizon_hold writes an observation's time into that statement as given, so the statement
 *  reads the observation exactly `seconds` old, however long the machine took to start it. */
const agedAtTheCheck = (observation: string, seconds: number): string =>
  `${observation.split(" ")[0]} extract(epoch from now()) - ${seconds}`;

// The bound itself, judged where no scheduling can move it: the holder is open past the bound and
// drew its id before the observer drew its own, and the judging statement reads the observation
// on its own clock (agedAtTheCheck), half a second under the bound and then half a second over it.
// A bound of 4.5 s or less names the holder at the first, and a bound over 5.5 s names none at the
// second. A check that waited on the real clock instead had only the time between its wait and its
// statement to spare, and a loaded machine spends more than that.
test.skipIf(!live)(
  "a transaction that drew its id before the observation is no holder until the observation has waited the bound",
  async () => {
    const pid = await sql("SELECT pg_backend_pid()");
    await sql("BEGIN", "SELECT pg_current_xact_id()");
    try {
      await untilPastTheBound(`(select xact_start from pg_stat_activity where pid = ${pid})`);
      expect(await heldPastTheBound(pid)).toBe("t");
      const observation = await observe();
      expect(
        await check(agedAtTheCheck(observation, boundSeconds - 0.5)),
        `backend ${pid} has held the observation back ${boundSeconds - 0.5} s, under the bound, however long it has been open`
      ).toEqual({ age: null, holder: "" });
      const over = await check(agedAtTheCheck(observation, boundSeconds + 0.5));
      expect(
        over.holder,
        `backend ${pid} has held the observation back ${boundSeconds + 0.5} s, past the bound`
      ).toStartWith(`backend ${pid} (application `);
      expect(over.age ?? Number.NaN, "the age of the observation the check judged").toBeCloseTo(
        boundSeconds + 0.5,
        3
      );
    } finally {
      await sql("ROLLBACK");
    }
  },
  rowLimit("short", "short", "short")
);

test.skipIf(!live)(
  "a transaction that drew its id before the observation and has held it back the bound is named",
  async () => {
    const pid = await sql("SELECT pg_backend_pid()");
    await sql("BEGIN", "SELECT pg_current_xact_id()");
    try {
      const observation = await observe();
      await untilPastTheBound(takenAt(observation));
      expect((await check(observation)).holder).toStartWith(`backend ${pid} (application `);
    } finally {
      await sql("ROLLBACK");
    }
  },
  rowLimit("short", "short")
);

// The driver starts the watch 5 s before it sends its event. A transaction that drew its id before
// the watch began is named when the watch's first observation has waited the bound, before the
// event is sent, however soon after the event it then ends. A watch begun at the event named it
// only if it held its id 5 s into the wait.
test.skipIf(!live)(
  "a transaction that drew its id before the watch began is named once the watch's first observation has waited the bound",
  async () => {
    const pid = await sql("SELECT pg_backend_pid()");
    await sql("BEGIN", "SELECT pg_current_xact_id()");
    try {
      const { code, stdout, stderr, named } = await runLib(
        'start_horizon_watch hold && printf "started, naming none"'
      );
      expect(code, `${named} exited ${code}, printing ${JSON.stringify(stdout)}: ${stderr}`).toBe(
        1
      );
      const failure = stderr.match(
        /^FAIL: hold no transaction of this run's database holds the event horizon back: (backend \d+ \(application [\s\S]*?) has held back an observation of the horizon that has waited (\d+(?:\.\d+)?) s on it/m
      );
      expect(failure, `start_horizon_watch's FAIL line names a holder: ${stderr}`).not.toBeNull();
      expect(failure?.[1]).toStartWith(`backend ${pid} (application `);
      expect(Number(failure?.[2])).toBeGreaterThanOrEqual(boundSeconds);
    } finally {
      await sql("ROLLBACK");
    }
  },
  rowLimit("short")
);

// With no holder the watch's start returns once its first observation has waited the bound, and
// not before: the driver sends its event only then. How many observations it took on the way is
// the machine's: a first judgement that starts the bound or more after its observation returns at
// once, with one.
test.skipIf(!live)(
  "start_horizon_watch returns once its first observation has waited the bound, where nothing holds the horizon back",
  async () => {
    const printed = await lib(
      `start_horizon_watch hold && printf "%s|%s" "$observation_age" "\${#horizon_observations[@]}"`
    );
    const age = Number(printed.split("|")[0]);
    expect(
      age,
      `the age of the watch's first observation as it returned, and the observations it kept: ${printed}`
    ).toBeGreaterThanOrEqual(boundSeconds);
  },
  rowLimit("short")
);

// A poll with no watch begun would judge against its own observations alone, the defect the row
// of a transaction that drew its id before the watch began pins, so lib.sh refuses it before it
// reaches a database.
test(
  "watch_horizon refuses to poll a watch that start_horizon_watch never began",
  async () => {
    const { code, stdout, stderr, named } = await runBounded({
      argv: ["bash", "-c", ". e2e/acceptance/lib.sh && watch_horizon hold"],
      bound: "short",
      cwd: dispatchPackage,
      env: { PATH: process.env.PATH ?? "", HOME: process.env.HOME ?? "" },
      from: import.meta.url,
    });
    expect(code, `${named} exited ${code}: ${stderr}`).toBe(2);
    expect(stderr).toBe("watch_horizon polls only a watch that start_horizon_watch began\n");
    expect(stdout).toBe("");
  },
  rowLimit("short")
);
