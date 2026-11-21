// The failed-room probe: a document's live room fails while a writer is already queued behind
// it, and that writer must be told (503 DOC_SERVICE_UNAVAILABLE) instead of waiting for a
// recovery that cannot finish while the failing request holds what the recovery needs. The
// second half is the recovery: the room reloads, the next write lands, and the browser's own
// paragraph is still in the document the server serves.
//
// It is opt-in, because it takes about 40 s of real time (a trigger holds a durable append for
// 20 s) and it cancels a Postgres backend, so it is not part of the suite CI runs:
//
//   DISPATCH_FAILED_ROOM_PROBE=1 bunx playwright test \
//     --config e2e/playwright.config.ts --project=chromium e2e/failed-room.e2e.ts
//
// Comparing a tree against another is what it is for: run it on this head and on the base, and
// read the two lines it prints for the queued writer.

import {
  baseUrl,
  createComment,
  createIssue,
  createProject,
  getArtifactText,
  userHeaders,
} from "./api";
import { documentEditor } from "./editor";
import { sql } from "./psql";
import { emptyDatabase, expect, rowKey, test } from "./test";
import { asUser } from "./users";

/** Holds the room's next durable append inside its `doc_updates` insert for this long, which is
 * how the probe gets a writer it can cancel while it owns the room. Every `doc_updates` insert
 * on this database sleeps while the trigger is installed, so `releaseTheRoom` runs in a
 * `finally` and again in `test.afterEach`. */
async function holdTheRoomFor(seconds: number): Promise<void> {
  await sql(`
    create or replace function failed_room_hold() returns trigger language plpgsql as $$
    begin
      perform pg_sleep(${seconds});
      return new;
    end $$;
  `);
  await sql(`
    drop trigger if exists failed_room_hold on doc_updates;
    create trigger failed_room_hold before insert on doc_updates
      for each row execute function failed_room_hold();
  `);
}

async function releaseTheRoom(): Promise<void> {
  await sql("drop trigger if exists failed_room_hold on doc_updates");
  await sql("drop function if exists failed_room_hold()");
}

// A trigger left behind makes every later `doc_updates` insert on this database sleep, which
// fails whatever writes a document next - `resetDatabase`'s wait for the tables it truncates
// included. This runs after a timeout, a Ctrl-C between tests, and a throw inside the body,
// which the `finally` below covers on its own.
test.afterEach(async () => {
  if (process.env.DISPATCH_FAILED_ROOM_PROBE === "1") {
    await releaseTheRoom();
  }
});

/** The backend the trigger is holding inside its `doc_updates` insert: the room's own durable
 * writer, which owns the room's advisory lock while it sleeps, so cancelling it fails the room
 * itself rather than one request's transaction. */
async function heldAppendPid(): Promise<string> {
  let pid = "";
  // Read until an append sleeps inside its insert: ygo flushes the browser's keystrokes about
  // twelve seconds after they were typed (the row below), and 25 s is the 250 reads 100 ms
  // apart this wait has always been given.
  await expect
    .poll(
      async () => {
        pid = await sql(`
          select pid from pg_stat_activity
          where datname = current_database()
            and wait_event = 'PgSleep'
            and query ilike '%doc_updates%'
          order by query_start limit 1
        `);
        return pid;
      },
      { intervals: [100], message: "no append was held in its insert", timeout: 25_000 }
    )
    .not.toBe("");
  return pid;
}

/** How many of this database's backends wait on a lock, as `count(*)`'s text: the anchored
 * writers queued behind the held append. The first waits on the document's advisory lock, which
 * the sleeping append holds (`readFork` taking `headUnderDocumentLock`, envoy's
 * `dispatch/docs/livewrite.go`); the one behind it waits on its issue's owner lock, which the
 * first took when it began (`lockCommentOwner`, `dispatch/api/comment_create.go`). The append
 * itself waits in `pg_sleep`, which Postgres reports as a timeout and not as a lock. */
async function writersQueued(): Promise<string> {
  return sql(`
    select count(*) from pg_stat_activity
    where datname = current_database() and wait_event_type = 'Lock'
  `);
}

/** How each wait for a writer to queue polls. A writer reaches its lock in one request's time,
 * and the two waits, with the pid read before them, have to end inside the 20 s the trigger holds
 * the append for: a writer that has not queued in 5 s will not have queued when the cancel comes. */
const QUEUE = { intervals: [50], timeout: 5_000 };

interface Attempt {
  readonly status: number;
  readonly code: string;
  readonly seconds: number;
}

async function anchoredComment(issueKey: string, body: string, quote: string): Promise<Attempt> {
  const started = Date.now();
  const response = await fetch(`${baseUrl}/api/v1/issues/${issueKey}/comments`, {
    body: JSON.stringify({ anchor: { artifact: "spec", quote }, body }),
    headers: { "Content-Type": "application/json", ...(await userHeaders("alice")) },
    method: "POST",
    signal: AbortSignal.timeout(30_000),
  });
  const text = await response.text();
  let code = "";
  try {
    code = (JSON.parse(text) as { code?: string }).code ?? "";
  } catch {
    code = text.slice(0, 40);
  }
  return { code, seconds: (Date.now() - started) / 1000, status: response.status };
}

test("a writer inside the docs layer when its room fails is told, and the room recovers", async ({
  browser,
}) => {
  const core = rowKey("CORE");
  test.skip(
    process.env.DISPATCH_FAILED_ROOM_PROBE !== "1",
    "the failed-room probe is opt-in: it holds a durable append for 20 s and cancels a backend"
  );
  test.setTimeout(240_000);
  // The `test` of this suite empties the database once per worker, not before each row
  // (e2e/test.ts), so this row empties it itself. This body is read as one procedure by hand, on
  // two checkouts at once (the header above), and what the probe starts from is the first thing a
  // reader of that comparison needs: a row whose 40 s are spent on one append cannot be read
  // against another tree's run without it. It empties through `emptyDatabase`, which counts the
  // database as the worker's, so the datasets the worker keeps are written again after it.
  await emptyDatabase();
  await createProject({ key: core, name: "Core" });
  const issue = await createIssue({
    project: core,
    spec: "## Plan\n\nShip the migration.",
    title: "Failed room probe",
  });
  await createComment(issue.key, { body: "seed" });

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  await page.goto(`/issues/${issue.key}/spec`);
  const editor = documentEditor(page);
  await expect(editor).toBeVisible();
  await expect(editor).toContainText("Ship the migration.");

  // The browser's own edit, so the room holds a live paragraph the recovery has to bring back -
  // and so the room's durable writer has something to append.
  await editor.click();
  await page.keyboard.press("End");
  await page.keyboard.type(" The browser typed this.");
  await expect(editor).toContainText("The browser typed this.");

  // The room's own durable writer is what has to fail, and it is the browser's append: ygo
  // persists the keystrokes about twelve seconds later, on the connection that holds the room's
  // advisory lock. The trigger goes in before that flush, so the append sleeps inside its insert
  // where a cancel can reach it; an anchored comment then queues behind the room, a second one
  // behind that, and only then is the sleeping append cancelled. The room fails with both
  // writers already inside the docs layer, waiting for what its recovery needs.
  let holding: Promise<Attempt>;
  let queued: Promise<Attempt>;
  await holdTheRoomFor(20);
  try {
    const pid = await heldAppendPid();
    holding = anchoredComment(issue.key, "the writer that arrives after the failure", "migration");
    // Each writer is sent once the one before it has queued, so the second queues behind the first.
    await expect
      .poll(writersQueued, { message: "the first writer never queued behind the room", ...QUEUE })
      .toBe("1");
    queued = anchoredComment(issue.key, "the writer behind it", "migration");
    await expect
      .poll(writersQueued, { message: "the second writer never queued behind the first", ...QUEUE })
      .toBe("2");
    const cancelled = await sql(`select pg_cancel_backend(${pid})`);
    console.log(`FAILED-ROOM cancelled the room's durable append pid=${pid} result=${cancelled}`);
  } finally {
    // Whatever happened above, no later `doc_updates` insert on this database may sleep.
    await releaseTheRoom();
  }

  const holder = await holding;
  console.log(
    `FAILED-ROOM first status=${holder.status} code=${holder.code} in ${holder.seconds}s`
  );
  const behind = await queued;
  console.log(
    `FAILED-ROOM queued status=${behind.status} code=${behind.code} in ${behind.seconds}s`
  );
  // The writer that was inside the docs layer when the room failed is told so, rather than
  // waiting for a recovery that cannot finish while it holds what that recovery needs until its
  // client gives up at 30 s.
  expect(holder.status).toBe(503);
  expect(holder.code).toBe("DOC_SERVICE_UNAVAILABLE");
  // The one behind it has two correct outcomes: told the same way, or admitted after the room
  // has already reloaded. Anything else - a hang, a 500 - is the failure this probe is about.
  expect([201, 503]).toContain(behind.status);
  if (behind.status === 503) {
    expect(behind.code).toBe("DOC_SERVICE_UNAVAILABLE");
  }

  // The recovery half: the room reloads from its durable copy, so a later write lands and the
  // browser's edit is in the document the server serves. The write is made once and then again a
  // second after each one that did not land, twenty times at most, which is what this probe has
  // always given the reload; every status is kept, so a reload that never ends reports them all.
  let recovered = await anchoredComment(issue.key, "after the recovery", "migration");
  const statuses = [recovered.status];
  // A loop and not `expect.poll`, whose timeout would count each write's own time in the bound.
  for (let attempt = 0; attempt < 20 && recovered.status !== 201; attempt += 1) {
    // A second between writes, the pace this probe has always retried at: the reload has no
    // signal of its own to wait on, and only a write that lands says it has finished.
    const { promise, resolve } = Promise.withResolvers<void>();
    setTimeout(resolve, 1_000);
    await promise;
    recovered = await anchoredComment(issue.key, `after the recovery ${attempt}`, "migration");
    statuses.push(recovered.status);
  }
  console.log(
    `FAILED-ROOM recovered status=${recovered.status} code=${recovered.code} in ${recovered.seconds}s`
  );
  expect(statuses).toContain(201);

  const text = await getArtifactText(issue.primary_artifact_id);
  console.log(`FAILED-ROOM document text=${JSON.stringify(text.markdown)}`);
  // The room reloaded from its durable copy with the browser's own paragraph in it, which is the
  // half of the recovery no status code shows.
  expect(text.markdown).toContain("The browser typed this.");
  await alice.close();
});
