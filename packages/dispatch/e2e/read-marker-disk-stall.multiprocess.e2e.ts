import { setTimeout as sleep } from "node:timers/promises";

import { createIssue, createProject } from "./api";
import { diskStallDir, diskStallVariable, endStall, stallDisk, stallLoaded } from "./disk-stall";
import { connectedDot, documentEditor } from "./editor";
import { sql } from "./psql";
import { resetDatabase } from "./seed";
import { expect, type Route, test } from "./test";
import { asUser } from "./users";

/**
 * The reset every row starts with (e2e/seed.ts) empties `user_document_reads`, and waits at most
 * 5 s for the table. A page that opens a document marks it read (PUT /api/v1/me/documents/{id}/
 * read), and a row that ends right after closes that page while the write is in flight. When the
 * database's disk stops answering at that moment, the write's commit waits for it: Postgres
 * flushes a commit record with its interrupts held, so the cancel the server sends when the page
 * goes (pgx's CancelRequest) does not end it, and the backend holds its lock on the table for as
 * long as the disk is silent. Merge-group run 44456291758 failed so, its next row's reset naming
 * `insert into user_document_reads ... (RowExclusiveLock, active)`.
 *
 * This row stalls the disk from the moment the page sends that write until the reset after it
 * has returned, for a minute at most, and requires the reset to empty the table all the same:
 * true only of a database whose commits do not wait for the disk, as the harness's own Postgres
 * does not (e2e/harness-postgres.ts). It needs a Postgres that can be stalled, which
 * `DISPATCH_E2E_PG_STALL_DIR` names (e2e/disk-stall.ts), and is skipped without one.
 */

const stallDir = diskStallDir();

/** The longest the row keeps the disk silent, whatever the reset does. */
const stallBoundMs = 60_000;

const readMarkPath = /^\/api\/v1\/me\/documents\/[^/]+\/read$/;

/** How many of the run's sessions other than this one are in a statement or a transaction. */
async function statementsInFlight(): Promise<number> {
  return Number(
    await sql(
      `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database()
         AND backend_type = 'client backend' AND state <> 'idle' AND pid <> pg_backend_pid()`
    )
  );
}

test("a read marker whose page closed while the database's disk did not answer holds nothing the next reset empties", async ({
  browser,
}) => {
  test.skip(
    stallDir === undefined,
    `${diskStallVariable} is unset, so this run's Postgres cannot be made to stop answering (e2e/disk-stall.ts)`
  );
  if (stallDir === undefined) return;
  test.setTimeout(2 * stallBoundMs);
  // The control: a run that names a stall directory its Postgres never loaded would stall
  // nothing and pass whatever the database does.
  expect(
    await stallLoaded(),
    `${diskStallVariable}=${stallDir} is set, but this run's Postgres did not load its fsync-stall.so`
  ).toBe(true);

  await createProject({ key: "STALL", name: "Disk stall" });
  const issue = await createIssue({
    project: "STALL",
    spec: "# Disk stall\n\nRead while the disk is silent.\n",
    title: "A document read as the disk stops answering",
  });

  const context = await asUser(browser, "alice");
  const bound = setTimeout(() => endStall(stallDir), stallBoundMs);
  try {
    const page = await context.newPage();
    const isReadMark = (method: string, url: string): boolean =>
      method === "PUT" && readMarkPath.test(new URL(url).pathname);
    // The first open marks the document read, as the earlier opens of run 44456291758's row did.
    const firstMark = page.waitForResponse((response) =>
      isReadMark(response.request().method(), response.url())
    );
    await page.goto(`/issues/${issue.key}/spec`);
    await expect(documentEditor(page)).toContainText("Read while the disk is silent.");
    expect((await firstMark).status()).toBe(200);

    // The open after it marks it again, and that mark is held at the page until the document has
    // opened, its room is connected and every statement of the stack has finished, so it is the
    // one write in flight when the disk stops answering. It moves the row the first one wrote,
    // which is the write that run's reset found holding `user_document_reads`.
    const held = Promise.withResolvers<Route>();
    let holding = false;
    await page.route("**/api/v1/me/documents/*/read", async (route) => {
      if (!isReadMark(route.request().method(), route.request().url()) || holding) {
        await route.continue();
        return;
      }
      holding = true;
      held.resolve(route);
    });
    const answered = page
      .waitForResponse((response) => isReadMark(response.request().method(), response.url()))
      .catch(() => undefined);
    await page.reload();
    await expect(documentEditor(page)).toContainText("Read while the disk is silent.");
    await expect(connectedDot(page)).toHaveText("connected");
    await expect.poll(() => holding, { message: "the page sent its second read mark" }).toBe(true);
    let quiet = 0;
    await expect
      .poll(
        async () => {
          quiet = (await statementsInFlight()) === 0 ? quiet + 1 : 0;
          return quiet;
        },
        { intervals: [100], message: "the stack's statements in flight all finished" }
      )
      .toBeGreaterThanOrEqual(3);
    stallDisk(stallDir);
    await (await held.promise).continue();
    // A second for the answer, which a database whose commit waits for the disk never sends;
    // then the row ends and its page goes, as every row's does.
    await Promise.race([answered, sleep(1_000)]);
    await context.close();

    // The next row's reset, with the disk still silent.
    await resetDatabase();
  } finally {
    clearTimeout(bound);
    endStall(stallDir);
    await context.close();
  }
});
