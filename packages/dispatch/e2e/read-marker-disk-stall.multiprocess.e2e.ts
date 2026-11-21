import { existsSync } from "node:fs";

import { createIssue, createProject } from "./api";
import {
  diskStallDir,
  diskStallVariable,
  endStall,
  stallEnvironment,
  stallServer,
} from "./disk-stall";
import { connectedDot, documentEditor, holdFirstRequest } from "./editor";
import { sql } from "./psql";
import { emptyDatabase, expect, rowKey, test } from "./test";
import { asUser } from "./users";

/**
 * The reset that empties the database (e2e/seed.ts) - once per worker, before its first row, and
 * where a row empties it itself (`emptyDatabase`, e2e/test.ts) - empties `user_document_reads`,
 * and waits at most 5 s for the table. A page that opens a document marks it read (PUT
 * /api/v1/me/documents/{id}/read), and a row that ends right after closes that page while the
 * write is in flight. When the database's disk stops answering at that moment, the write's commit
 * waits for it: Postgres flushes a commit record with its interrupts held, so the cancel the
 * server sends when the page goes (pgx's CancelRequest) does not end it, and the backend holds
 * its lock on the table for as long as the disk is silent. Merge-group run 44456291758 failed so,
 * when the database was still emptied before every row: its next row's reset named
 * `insert into user_document_reads ... (RowExclusiveLock, active)`.
 *
 * This row stalls the disk as the page sends that write, and keeps it stalled until the row ends:
 * only the cleanup lets it go, so a reset that waits for the disk to answer waits out the row's
 * own timeout and fails. Before the page closes, the write has reached the database: either its
 * answer came, or a backend is committing it and waits on the disk. An answer counts only from a
 * server the stall holds, which the row reads on that server as it stalls the disk, before it
 * lets the write go (`stallServer`). The reset must then empty the table with the disk still
 * silent, true only of a database whose commits do not wait for the disk, as the harness's own
 * Postgres does not (e2e/harness-postgres.ts). It needs a Postgres that can be stalled, which
 * `DISPATCH_E2E_PG_STALL_DIR` names (e2e/disk-stall.ts), and is skipped without one, except on
 * GitHub Actions, where `GITHUB_ACTIONS` is "true" in every step: CI's multiprocess job names one,
 * and a run of it that lost the variable would skip the one row that holds it to the reset, and
 * pass, so there the row fails instead. `CI` is not read, since an agent's shell sets `CI=true`
 * too; this is the rule packages/envoy/internal/testpg/testdb/skip.go's `SkipOutsideCI` keeps.
 */

const stallDir = diskStallDir();

const readMarkPath = /\/api\/v1\/me\/documents\/[^/]+\/read$/u;

/** How many of the run's sessions other than this one are in a statement or a transaction. */
async function statementsInFlight(): Promise<number> {
  return Number(
    await sql(
      `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database()
         AND backend_type = 'client backend' AND state <> 'idle' AND pid <> pg_backend_pid()`
    )
  );
}

/** How many backends are committing a read mark and waiting for the disk to take it: the one
 *  flushing the WAL waits in `IO:WalSync`, and a commit behind it waits for its turn in
 *  `LWLock:WALWrite`, as a read mark that reaches the server after another write began its flush
 *  does. A commit in either wait takes no cancel. */
async function readMarksWaitingOnTheDisk(): Promise<number> {
  return Number(
    await sql(
      `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database()
         AND state = 'active'
         AND ((wait_event_type = 'IO' AND wait_event = 'WalSync')
           OR (wait_event_type = 'LWLock' AND wait_event = 'WALWrite'))
         AND position('insert into user_document_reads' in query) > 0`
    )
  );
}

// The stall's one release, whatever ended the row: a timeout abandons the row's body, and the
// hook still runs.
test.afterEach(() => {
  if (stallDir !== undefined) endStall(stallDir);
});

test("a read marker whose page closed while the database's disk did not answer holds nothing the next reset empties", async ({
  browser,
}) => {
  const stall = rowKey("STALL");
  if (stallDir === undefined && process.env.GITHUB_ACTIONS === "true") {
    throw new Error(
      `${diskStallVariable} is unset on GitHub Actions: CI runs this row against a Postgres it ` +
        "can stall (e2e/disk-stall.ts), so a CI run without one fails rather than skips it"
    );
  }
  test.skip(
    stallDir === undefined,
    `${diskStallVariable} is unset, so this run's Postgres cannot be made to stop answering (e2e/disk-stall.ts)`
  );
  if (stallDir === undefined) return;
  test.setTimeout(120_000);

  await createProject({ key: stall, name: "Disk stall" });
  const issue = await createIssue({
    project: stall,
    spec: "# Disk stall\n\nRead while the disk is silent.\n",
    title: "A document read as the disk stops answering",
  });

  const context = await asUser(browser, "alice");
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
    // which is the write that run's reset found holding `user_document_reads`. The page's GET of
    // the same path, its read of the mark, is not held.
    const mark = await holdFirstRequest(page, readMarkPath, { method: "PUT" });
    let arrived = false;
    void mark.held.then(() => {
      arrived = true;
    });
    let answer: number | undefined;
    void page
      .waitForResponse((response) => isReadMark(response.request().method(), response.url()))
      .then((response) => {
        answer = response.status();
      })
      .catch(() => undefined);
    await page.reload();
    await expect(documentEditor(page)).toContainText("Read while the disk is silent.");
    await expect(connectedDot(page)).toHaveText("connected");
    await expect.poll(() => arrived, { message: "the page sent its second read mark" }).toBe(true);
    const held = await mark.held;
    expect(held.request().method(), "the request held is the read mark").toBe("PUT");
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
    // The stall, and its control: an answer to the mark below is evidence only from a server the
    // stall holds. A run that names a stall its Postgres does not wait on, whether that server
    // never loaded the library or watches another file, would stall nothing and pass whatever
    // the database does.
    expect(
      await stallServer(stallDir),
      `${diskStallVariable}=${stallDir} is set, and the stall it names does not hold this run's Postgres`
    ).toEqual([]);
    await held.continue();

    // The write has reached the database before its page goes: it was answered, which a
    // database whose commits wait for the disk cannot do now, or a backend is committing it and
    // waits on the silent disk. A mark the page dropped before it reached the server, which is
    // nothing the reset has to wait out, is neither, and fails here.
    await expect
      .poll(
        async () => {
          if (answer !== undefined) return `answered ${answer}`;
          return (await readMarksWaitingOnTheDisk()) > 0 ? "committing on the silent disk" : "";
        },
        {
          intervals: [100],
          message:
            "the read mark reached the database: answered 200, or its commit waits on the disk",
        }
      )
      .toMatch(/^(answered 200|committing on the silent disk)$/);
    // Then the row ends and its page goes, as every row's does.
    await context.close();

    // A reset of the database, with the disk still silent: the one the next worker on this stack
    // starts with, or one a row makes itself. A reset that needs the disk waits on it until the
    // row's timeout; only the cleanup ends the stall. It is counted as the worker's
    // (`emptyDatabase`), so the datasets the worker keeps are written again into the database
    // the next row reads: the delivery settings `two-builds.multiprocess.e2e.ts` reads after it.
    await emptyDatabase();
    expect(
      existsSync(stallEnvironment(stallDir).PG_STALL_FILE),
      "the disk was still stalled when the reset returned"
    ).toBe(true);
  } finally {
    endStall(stallDir);
    await context.close();
  }
});
