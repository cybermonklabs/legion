/** The control for the reset: a table the reset is never told about.
 *
 *  `e2e/seed.ts` reads the tables it empties from the schema and keeps a short list of the ones
 *  it must not empty. These two rows hold it to the reading a list of names cannot give: a table
 *  nothing in `e2e/seed.ts` names, and nothing else reads, holds no row an earlier scenario
 *  wrote. The first row writes one marker row into it. The second row reads the same table at its
 *  own start and finds it empty, because the reset between them emptied a table no one had
 *  written down.
 *
 *  Against the list this replaced, the second row fails: that list named its tables by hand and
 *  widened itself by a recursive walk over the foreign keys out of them, the control table
 *  carries no foreign key, and so the marker row stood until the stack went away.
 *
 *  The table is created here rather than in a migration because this harness has no schema of its
 *  own. Each stack's database carries the server's schema alone, applied at boot from
 *  `packages/envoy/internal/dispatch/store/migrations`, and a migration added there would ship a
 *  test's table to every production database. The create runs in the first row, under `IF NOT
 *  EXISTS`, so a stack that already carries the table pays one statement that changes nothing.
 *
 *  Neither row drives a browser, and both stand on a named exemption in `e2e/no-browser-rows.ts`:
 *  what they read is what this suite's own fixture does between two of its rows, which no other
 *  layer runs.
 */

import { sql } from "./psql";
import { expect, test } from "./test";

/** The two rows must run in this order and against one stack: the first writes the row whose
 *  absence the second reads, so a run that scheduled them in parallel, or in two stacks, would
 *  read a table nothing had written into and pass on nothing. Serial mode keeps the file in one
 *  worker, and a worker runs one stack. */
test.describe.configure({ mode: "serial" });

/** The control table: no foreign key, no trigger, and no reader but these two rows. A foreign key
 *  is what the old list widened itself by, so the one thing this table must not carry is a column
 *  that points at another table. */
const createControlTable = `CREATE TABLE IF NOT EXISTS reset_control_marker (
  id bigserial PRIMARY KEY,
  written_by text NOT NULL
)`;

/** What the two rows read: the number of rows the control table holds now. */
const markerRows = `SELECT count(*) FROM reset_control_marker`;

test("a marker row written into a table the reset never names stays for the rest of its scenario", async () => {
  await sql(
    createControlTable,
    `INSERT INTO reset_control_marker (written_by) VALUES ('the row that writes the marker')`
  );

  expect(Number(await sql(markerRows))).toBe(1);
});

test("the scenario after it starts with that table empty, with nobody naming the table", async () => {
  expect(Number(await sql(markerRows))).toBe(0);
});
