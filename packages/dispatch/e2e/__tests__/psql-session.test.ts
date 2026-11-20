import { afterAll, beforeAll, expect, test } from "bun:test";
import { randomUUID } from "node:crypto";
import { captureEnvironment } from "@legion/test-fixtures/environment";

import { dropDatabase, resetSession, sql, sqlAt } from "../psql";
import { bounds, eventually, rowLimit } from "./bounded-subprocess";
import {
  missingForServer,
  requireNothingMissing,
  serverVariable,
  skipsWithout,
} from "./postgres-provider";

/** The PostgreSQL server this run may make a database on: an issue pod names it, a developer
 *  names it, and the `dispatch-ui` CI job names its Postgres service. The session protocol is
 *  psql's answer to real statements, so the cases that need a database are skipped where no
 *  server is named rather than faked against a mock psql that could agree with a wrong reading
 *  of the protocol: skipped on a developer's run, and failed in CI, which never skips them
 *  (`e2e/__tests__/postgres-provider.ts`). */
const server = process.env[serverVariable] ?? "";
/** Everything those cases need and this host or run does not have, in one list, so a host that
 *  has neither psql nor a server reads one line naming both. */
const missing = missingForServer("a server this run may create a database on");
const live = missing.length === 0;
const skipped = skipsWithout("e2e/__tests__/psql-session.test.ts", missing);

/** The database of this run alone, made in `beforeAll` and dropped in `afterAll`. The cases here
 *  create temporary tables and read pg_stat_activity, so they run on a database nobody else is
 *  using rather than on whatever a developer's own DATABASE_URL happens to name. `create
 *  database` copies template1, which carries the `vector` extension on the servers this runs on,
 *  and nothing here needs it anyway. */
const database = `dispatch_psql_session_${randomUUID().replaceAll("-", "_")}`;
const databaseUrl = (): string => {
  const url = new URL(server);
  url.pathname = `/${database}`;
  return url.href;
};

/** Puts back what the environment held before this file pointed `sql` at its own database. */
let restoreNames: (() => void) | undefined;

beforeAll(
  async () => {
    if (!live) return;
    await sqlAt(server, [`create database ${database}`]);
    // `sql` resolves its database from the environment when it starts its psql, so this is where
    // the run's own database replaces whatever the caller's environment named. A deployed URL wins
    // over DATABASE_URL in that resolution, so both names of that pair go.
    restoreNames = captureEnvironment([
      "DATABASE_URL",
      "PLAYWRIGHT_BASE_URL",
      "PLAYWRIGHT_DATABASE_URL",
    ]);
    process.env.DATABASE_URL = databaseUrl();
    delete process.env.PLAYWRIGHT_BASE_URL;
    delete process.env.PLAYWRIGHT_DATABASE_URL;
  },
  // This hook makes the run's own database with `sqlAt`, whose psql e2e/psql.ts starts for
  // e2e/harness-postgres.ts and e2e/local-instance.ts as well, so that child is bounded nowhere
  // and the minute here is what stops a create that hangs.
  60_000
);

afterAll(
  async () => {
    restoreNames?.();
    // The psql this file's cases started is connected to the database dropped below, and lives in
    // e2e/psql.ts for as long as the process does: left open, it would answer the next `sql` call
    // of this process - a rerun of this file, or another file - from a database that is gone.
    resetSession();
    if (!live) return;
    // The psql killed above may still be on the server; `dropDatabase` ends it (its doc says how).
    await dropDatabase(server, database);
  },
  // This hook drops that database with `dropDatabase`, whose psql is `sqlAt`'s, which two modules
  // outside this directory call too, so bounding its psql would reach past this file. The minute
  // here is what stops a drop that waits forever on a connection.
  60_000
);

// `sql` resolves the database when it starts its psql, and once an earlier case has started one,
// no call reaches that refusal again; so the case ends that psql first. It is the refusal of this
// process's own `sql` that it proves, so the case runs here rather than in a child, and it puts
// the two names back rather than replacing `process.env`: a replaced object is no longer the
// runtime's environment, so every later `Bun.spawn` of the run would inherit the copy's values
// and a later write would reach neither the real environment nor `Bun.env`.
test("refuses when neither PLAYWRIGHT_DATABASE_URL nor DATABASE_URL names a database", async () => {
  resetSession();
  const restore = captureEnvironment(["DATABASE_URL", "PLAYWRIGHT_DATABASE_URL"]);
  delete process.env.DATABASE_URL;
  delete process.env.PLAYWRIGHT_DATABASE_URL;
  try {
    await expect(sql("SELECT 1")).rejects.toThrow(
      "PLAYWRIGHT_DATABASE_URL or DATABASE_URL must name the database for this e2e run"
    );
  } finally {
    restore();
  }
});

// A temporary table lives in its own connection, so one that answers a later call is itself the
// proof that every call ran on the one psql this process started.
test.skipIf(skipped)(
  "stops at the first failing statement, on a session that outlives the call",
  async () => {
    requireNothingMissing(missing);
    await sql("CREATE TEMP TABLE statement_log (ran int)");

    await expect(
      sql(
        "INSERT INTO statement_log VALUES (1)",
        "SELECT nosuchcolumn",
        "INSERT INTO statement_log VALUES (3)"
      )
    ).rejects.toThrow(/nosuchcolumn/);

    expect(
      await sql("SELECT coalesce(string_agg(ran::text, ',' ORDER BY ran), '') FROM statement_log")
    ).toBe("1");
    expect(await sql("SELECT 3")).toBe("3");
  }
);

// A table of its own, holding a row committed before the failed call, so the case reads only what
// it wrote whichever case ran before it.
test.skipIf(skipped)(
  "rolls back the transaction a failure aborted before the next call",
  async () => {
    requireNothingMissing(missing);
    await sql("CREATE TEMP TABLE rollback_log (ran int)", "INSERT INTO rollback_log VALUES (1)");
    await expect(
      sql(
        "BEGIN",
        "INSERT INTO rollback_log VALUES (2)",
        "SELECT nosuchcolumn",
        "COMMIT",
        "INSERT INTO rollback_log VALUES (4)"
      )
    ).rejects.toThrow(/nosuchcolumn/);

    // Without the ROLLBACK the session sends, this INSERT would be refused as part of an aborted
    // transaction; with it, the insert lands and nothing of the failed call did.
    await sql("INSERT INTO rollback_log VALUES (5)");
    expect(
      await sql("SELECT coalesce(string_agg(ran::text, ',' ORDER BY ran), '') FROM rollback_log")
    ).toBe("1,5");
  }
);

test.skipIf(skipped)("keeps each caller's statements together when two calls overlap", async () => {
  requireNothingMissing(missing);
  const [first, second] = await Promise.all([
    sql("SELECT 'a1'", "SELECT 'a2'"),
    sql("SELECT 'b1'", "SELECT 'b2'"),
  ]);
  expect(first).toBe("a1\na2");
  expect(second).toBe("b1\nb2");
});

// e2e/keyed-writes.e2e.ts holds a row locked in one call while it watches pg_stat_activity for
// that wait in another, so an overlapping call must reach the database beside the session's
// transaction rather than behind it.
test.skipIf(skipped)(
  "runs a call that overlaps another on a second connection",
  async () => {
    requireNothingMissing(missing);
    await sql("CREATE TABLE overlap_release (released int)");
    // The held statement runs until a row of `overlap_release` is committed, which only a call
    // that runs beside it can write. Once the short bound has passed it stops waiting and fails,
    // naming the table, so a call queued behind it fails this row by name rather than hanging.
    const opening = "DO $$ BEGIN WHILE NOT EXISTS (SELECT FROM overlap_release)";
    const held =
      `${opening} LOOP IF clock_timestamp() > statement_timestamp() + interval '${bounds.short} ms' ` +
      "THEN RAISE EXCEPTION 'no overlapping call wrote overlap_release'; END IF; " +
      "PERFORM pg_sleep(0.01); END LOOP; END $$";
    const holding = sql("BEGIN", held, "COMMIT");
    // The session's statement reaches the server in its own time, which raises no event here, so
    // the overlapping call is made until it sees that statement running. It reads this run's
    // database alone, since other runs' statements share the server's activity view.
    try {
      await eventually(
        "an overlapping call to see the session's held statement running",
        async () => {
          const running = await sql(
            "SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() " +
              `AND state = 'active' AND query LIKE '${opening}%'`
          );
          return running === "1";
        }
      );
      await sql("INSERT INTO overlap_release VALUES (1)");
    } catch (error) {
      // The held statement gives up once the short bound has passed since it reached the
      // server. It is awaited here so that neither its rejection nor its connection reaches a
      // later row, and this row reports its own failure.
      await holding.catch(() => {});
      throw error;
    }
    await holding;
  },
  // The one wait this row ends at a bound itself: the overlapping call seeing the session's
  // statement, which the short bound of `eventually` ends. The held statement gives up once that
  // same bound has passed since it reached the server, a moment after that wait began and well
  // inside the 30 000 ms the limit adds, so the row fails on its error and not on this clock.
  rowLimit("short")
);

// A NOTICE and a WARNING go to the same pipe an error does, so what a statement wrote there is
// not what it is judged by: e2e/failed-room.e2e.ts drops a trigger that is not there between its
// rows, and Postgres answers that with a NOTICE.
test.skipIf(skipped)("keeps a statement psql only wrote a notice or a warning for", async () => {
  requireNothingMissing(missing);
  await sql("CREATE TEMP TABLE notice_log (ran int)");
  // psql prints a command's own tag before the next statement's rows, `-tA` and all.
  expect(await sql("DROP TRIGGER IF EXISTS never_made ON notice_log; SELECT 'dropped'")).toBe(
    "DROP TRIGGER\ndropped"
  );
  expect(
    await sql("DO $$ BEGIN RAISE WARNING 'a warning is not a failure'; END $$", "SELECT 'warned'")
  ).toBe("DO\nwarned");
});

// Both spellings a caller sends: a statement it terminated itself leaves psql an empty query
// after it, whose own verdict is success, and one it left open is run by the terminator the
// session appends.
test.skipIf(skipped)("rejects a statement that really failed with psql's own text", async () => {
  requireNothingMissing(missing);
  await expect(sql("SELECT nosuchcolumn")).rejects.toThrow(/nosuchcolumn/);
  await expect(sql("SELECT nosuchcolumn;")).rejects.toThrow(/nosuchcolumn/);
  expect(await sql("SELECT 'alive'")).toBe("alive");
});
