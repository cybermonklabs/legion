// The failed-room probe: a document's live room fails while a writer is already queued behind
// it, and that writer must be told (503 DOC_SERVICE_UNAVAILABLE) instead of waiting for a
// recovery that cannot finish while the failing request holds what the recovery needs. The
// second half is the recovery: the room reloads, the next write lands, and the browser's own
// paragraph is still in the document the server serves.
//
// It runs in `chromium-alone`, one row at a time on a machine of its own, on the full lane of
// .github/workflows/dispatch-browser.yaml: a trigger holds a durable append for up to 20 s, it
// cancels a Postgres backend, and what it asserts rests on the clock - the writer is told before
// its client gives up at 30 s - which three other workers on the machine would move
// (`clockSpecNames` in e2e/configurations.ts). The lines it prints for the queued writer are what
// a run on a change and a run on its base are read against.

import {
  baseUrl,
  createComment,
  createIssue,
  createProject,
  getArtifactText,
  quiesceDocuments,
  userHeaders,
} from "./api";
import { connectedDot, documentEditor } from "./editor";
import { sql } from "./psql";
import { resetDatabase } from "./seed";
import { expect, test } from "./test";
import { asUser } from "./users";

/** Holds the next `doc_updates` insert on this database inside the insert for this long - the
 * room's next durable append - which is how the probe gets a writer it can cancel while it owns
 * the room. That insert alone sleeps: the trigger sleeps on the first value of a sequence, which
 * the cancelled insert's rollback does not give back, so every later insert - the failed room's
 * own flush, the writers' appends, the recovery's - runs at once with the trigger still on the
 * table, and nothing is taken off `doc_updates` while the probe's writers are inside their
 * transactions (`releaseTheRoom` says why nothing may be). */
async function holdTheRoomFor(seconds: number): Promise<void> {
  await releaseTheRoom();
  await sql("create sequence failed_room_hold_turn");
  await sql(`
    create function failed_room_hold() returns trigger language plpgsql as $$
    begin
      if nextval('failed_room_hold_turn') = 1 then
        perform pg_sleep(${seconds});
      end if;
      return new;
    end $$;
  `);
  await sql(`
    create trigger failed_room_hold before insert on doc_updates
      for each row execute function failed_room_hold();
  `);
}

/** Takes the hold off the database: its trigger, its function and its sequence. Dropping the
 * trigger locks `doc_updates` in ACCESS EXCLUSIVE mode, and while that lock is asked for, every
 * later read of the table waits behind the request. A document write holds the table from its
 * read of the head (ACCESS SHARE) to its end, and can wait in the server, where Postgres sees no
 * wait, for a read of the stored document on another connection (`readFork`, `coldFork` in
 * packages/envoy/internal/dispatch/docs/livewrite.go). A drop asked for between the two holds that
 * read until the write's client gives up: the probe's first writer once answered 500 after 30 s
 * that way, its read of the stored updates cancelled, when this drop ran in the body while that
 * writer was inside its transaction. So the drop runs before the hold goes on and after the body,
 * never while the probe's writers are in flight, and waits for its lock 100 ms at a time
 * (lock_timeout), holding nothing between tries, as the database reset's locks do (e2e/seed.ts)
 * and every migration's (`LockTimeout` in packages/envoy/internal/pgmigrate/lock.go); a table
 * still held after 10 s fails it, naming the holders. */
async function releaseTheRoom(): Promise<void> {
  await sql(
    `DO $$
      DECLARE
        deadline timestamptz := clock_timestamp() + interval '10 seconds';
        holders text;
      BEGIN
        PERFORM set_config('lock_timeout', '100ms', true);
        LOOP
          -- A failed try rolls back this block, releasing every lock it took.
          BEGIN
            DROP TRIGGER IF EXISTS failed_room_hold ON doc_updates;
            DROP FUNCTION IF EXISTS failed_room_hold();
            DROP SEQUENCE IF EXISTS failed_room_hold_turn;
            EXIT;
          EXCEPTION WHEN lock_not_available THEN
            IF clock_timestamp() >= deadline THEN
              SELECT string_agg(DISTINCT format('%s (%s, %s)',
                       left(regexp_replace(a.query, '\\s+', ' ', 'g'), 120), l.mode, a.state), '; ')
                INTO holders
                FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
                WHERE l.granted AND l.pid <> pg_backend_pid() AND l.relation = 'doc_updates'::regclass;
              RAISE EXCEPTION 'could not lock doc_updates to take the hold off within 10 s; held by: %',
                coalesce(holders, 'a transaction that ended just before this report');
            END IF;
          END;
        END LOOP;
      END
    $$`
  );
}

// A hold left behind keeps its trigger on `doc_updates` and, should no insert have taken its turn,
// sleeps the next insert on this database, which fails whatever writes a document next -
// `resetDatabase`'s wait for the tables it empties included. This runs after a timeout, a Ctrl-C
// between tests, a throw inside the body, and a body that passed.
test.afterEach(async () => {
  await releaseTheRoom();
});

/** The backend the trigger is holding inside its `doc_updates` insert: the room's own durable
 * writer, which owns the room's advisory lock while it sleeps, so cancelling it fails the room
 * itself rather than one request's transaction. */
async function heldAppendPid(): Promise<string> {
  for (let attempt = 0; attempt < 250; attempt += 1) {
    const pid = await sql(`
      select pid from pg_stat_activity
      where datname = current_database()
        and wait_event = 'PgSleep'
        and query ilike '%doc_updates%'
      order by query_start limit 1
    `);
    if (pid !== "") {
      return pid;
    }
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  throw new Error("no append was held in its insert");
}

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
  test.setTimeout(240_000);
  // The `test` of this suite empties the database before every row (e2e/test.ts), so this
  // repeats it. It is kept because what the probe starts from is the first thing a reader needs
  // who holds its `FAILED-ROOM` lines against another tree's: a row whose time is spent on one
  // held append cannot be read against another tree's run without it.
  await resetDatabase();
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
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

  // The browser's own edit, so the room holds a live paragraph the recovery has to bring back.
  await editor.click();
  await page.keyboard.press("End");
  await page.keyboard.type(" The browser typed this.");
  await expect(editor).toContainText("The browser typed this.");

  // The room stores a browser's keystrokes as they come, about 50 ms after each (roomAppendLinger
  // in packages/envoy/internal/dispatch/docs/room_appends.go), so a hold put on now could catch an
  // append of this paragraph itself, and the failed room would lose the rest of it until the
  // browser reconnects, up to 2 s later (RECONNECT_SPREAD_MS), and sends it again. Closing every
  // live document stores what each room holds first (quiesceDocuments), so the paragraph is
  // durable before anything fails, and the editor reconnects to a room loaded from that copy.
  const reopened = page.waitForEvent("websocket", (socket) =>
    new URL(socket.url()).pathname.startsWith("/ws/doc/")
  );
  await quiesceDocuments();
  await reopened;
  await expect(connectedDot(page)).toHaveText("connected");

  // The room's own durable writer is what has to fail, and it is the browser's append: the trigger
  // goes in, then the browser types once more, so that append sleeps inside its insert where a
  // cancel can reach it; an anchored comment then queues behind the room, a second one behind
  // that, and only then is the sleeping append cancelled. The room fails with both writers already
  // inside the docs layer, waiting for what its recovery needs. What the browser typed after the
  // paragraph is the failed room's to lose; the paragraph is in the copy it reloads from.
  let holding: Promise<Attempt>;
  let queued: Promise<Attempt>;
  await holdTheRoomFor(20);
  try {
    await editor.click();
    await page.keyboard.press("End");
    await page.keyboard.type(" Then this.");
    const pid = await heldAppendPid();
    holding = anchoredComment(issue.key, "the writer that arrives after the failure", "migration");
    await new Promise((resolve) => setTimeout(resolve, 1000));
    queued = anchoredComment(issue.key, "the writer behind it", "migration");
    await new Promise((resolve) => setTimeout(resolve, 1000));
    const cancelled = await sql(`select pg_cancel_backend(${pid})`);
    console.log(`FAILED-ROOM cancelled the room's durable append pid=${pid} result=${cancelled}`);
  } finally {
    // Whatever happened above, no later `doc_updates` insert on this database may sleep: this
    // takes the hold's one turn, should no insert have taken it. It touches the sequence alone,
    // and nothing of `doc_updates`, whose writers are inside their transactions now; the hold
    // comes off after the body (`releaseTheRoom`).
    await sql("select nextval('failed_room_hold_turn')");
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
  // browser's edit is in the document the server serves.
  let recovered = await anchoredComment(issue.key, "after the recovery", "migration");
  for (let attempt = 0; attempt < 20 && recovered.status !== 201; attempt += 1) {
    await new Promise((resolve) => setTimeout(resolve, 1000));
    recovered = await anchoredComment(issue.key, `after the recovery ${attempt}`, "migration");
  }
  console.log(
    `FAILED-ROOM recovered status=${recovered.status} code=${recovered.code} in ${recovered.seconds}s`
  );
  expect(recovered.status).toBe(201);

  const text = await getArtifactText(issue.primary_artifact_id);
  console.log(`FAILED-ROOM document text=${JSON.stringify(text.markdown)}`);
  // The room reloaded from its durable copy with the browser's own paragraph in it, which is the
  // half of the recovery no status code shows.
  expect(text.markdown).toContain("The browser typed this.");
  await alice.close();
});
