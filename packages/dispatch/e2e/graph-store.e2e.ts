/**
 * The node and tuple store, where a browser reads it: a supertag migration moving under an open
 * editor, and one person's page of what they wrote and what reached them.
 *
 * Both rows read the store through the server's own reads. What the store holds is written
 * through the real API wherever this harness can reach it, and in SQL exactly where it cannot -
 * each such place is named where it is written, with what writes it in a deployment.
 */
import { setTimeout as sleep } from "node:timers/promises";

import { expect, type Page, test } from "@playwright/test";
import { setLiveSessions } from "./agents";
import {
  createComment,
  createIssue,
  createProject,
  getArtifact,
  getArtifactText,
  getArtifactVersion,
  getIssue,
} from "./api";
import {
  ADDED_TYPE,
  applySupertagMigration,
  builtSchemaVersion,
  documentWithAddedType,
  installedVersion,
  listeners,
  priorSchemaVersion,
  processStartTimes,
  restoreStoreSchema,
  servesAddedType,
  storeSchemaBeforeTheChange,
} from "./block-schema";
import { connectedDot, countDocumentSockets, documentEditor, typeAtEnd } from "./editor";
import { sql } from "./psql";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/** The session the fake Envoy holds live, which a mention is delivered to. */
const SESSION = "019a1c09-7f0e-7000-8000-0000000000e1";

/** The writer the store's own delivery writer carries (`outbox.graphWriter`). */
const OUTBOX_WRITER = `'{"kind": "system", "id": "dispatch-outbox"}'::jsonb`;

test.beforeEach(async () => {
  await resetDatabase();
  // The reset keeps the schema's own nodes (`e2e/seed.ts`), so a row that moved them puts them
  // back here as well as at its end: a row that failed before its own restore must not leave the
  // next one on another schema.
  await restoreStoreSchema();
});

test.afterEach(async () => {
  await restoreStoreSchema();
  // The server serves the schema it installed, so the next spec reads the shipped one only once
  // this process has read the restore's announcement.
  await expect.poll(() => installedVersion()).toBe(builtSchemaVersion);
});

/** The document this run types in: an issue's spec, opened at `/issues/<key>/spec`. */
async function issueWithASpec(title: string): Promise<{ artifact: string; key: string }> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec: "The first line.", title });
  const spec = issue.primary_artifact_id;
  if (spec === undefined || spec === null) {
    throw new Error(`the issue ${issue.key} has no spec document`);
  }
  return { artifact: spec, key: issue.key };
}

/** Uploads `markdown` as a document of the issue through the tab's own fetch: the person's cookie
 *  and the dashboard's origin, as every write the page makes carries. It answers the status and
 *  the body, so a refusal is read rather than thrown. */
function writeThroughTheTab(
  page: Page,
  issueKey: string,
  name: string,
  markdown: string
): Promise<{ body: string; status: number }> {
  return page.evaluate(
    async ([key, documentName, content]) => {
      const response = await fetch(`/api/v1/issues/${key}/artifacts`, {
        body: JSON.stringify({ content, name: documentName }),
        headers: { "Content-Type": "application/json" },
        method: "POST",
      });
      return { body: await response.text(), status: response.status };
    },
    [issueKey, name, markdown] as const
  );
}

/** The document's newest stored version, read through the API as it stands. A settlement writes
 *  live typing as a version a short delay after the room's last update (`run-server.sh`), and
 *  quiescing the documents would cancel a settlement still pending rather than run it, so a
 *  caller polls this read until the settlement it waits for has written. */
async function newestStoredMarkdown(artifact: string): Promise<string> {
  const newest = ((await getArtifact(artifact)).versions ?? []).at(-1);
  if (newest === undefined) {
    throw new Error(`the document ${artifact} has no settled version`);
  }
  return (await getArtifactVersion(artifact, newest.number)).markdown;
}

// SLOW(>=2s): one live editor across a supertag migration - the store moved under the running
// server, the socket closed and opened again, and a settled version read back.
test("a supertag change leaves an open editor writing", async ({ browser }, testInfo) => {
  const { artifact, key } = await issueWithASpec("A supertag change");
  // The store before the change: the version below this build's, with the added type absent. The
  // page's own schema read then presents that version to the socket it opens.
  await storeSchemaBeforeTheChange();
  await expect.poll(() => installedVersion()).toBe(priorSchemaVersion);
  const startedBefore = await processStartTimes();
  expect(startedBefore).not.toBe("");

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const sockets = countDocumentSockets(page, artifact);
    await page.goto(`/issues/${key}/spec`);
    await expect(documentEditor(page)).toContainText("The first line.");
    await expect(connectedDot(page)).toHaveText("connected");
    await typeAtEnd(page, "Typed before the switch.");
    // Read back from the page, and from the server the socket wrote to.
    await expect(documentEditor(page)).toContainText("Typed before the switch.");
    await expect
      .poll(async () => (await getArtifactText(artifact)).markdown)
      .toContain("Typed before the switch.");

    // The migration, with this process's listener stopped in the same psql session right before
    // the commit: the notification reaches nobody, so the process holds the schema it has until
    // its hub redials, and the test can read that it is still behind.
    const [listener] = await listeners();
    expect(listener, "the server process names its listener connection").toBeDefined();
    await applySupertagMigration({ stopListenerOf: listener });

    // The transition: between the migration's commit and the socket's return the process is
    // still on the old schema, so a document holding a block of the new type is refused rather
    // than stored under it. The write goes through the tab, whose socket is still the one it
    // opened before the change.
    expect(await installedVersion()).toBe(priorSchemaVersion);
    const refused = await writeThroughTheTab(
      page,
      key,
      "during-the-switch.md",
      documentWithAddedType("Written while the process was behind.")
    );
    expect(refused.status, `the write during the transition answered ${refused.body}`).toBe(400);
    expect(refused.body).toContain(`unknown typed block \\"${ADDED_TYPE}\\"`);
    expect(((await getIssue(key)).artifacts ?? []).map((each) => each.name)).not.toContain(
      "during-the-switch.md"
    );

    // The process reinstalls after its next LISTEN rather than staying behind for ever: its hub
    // redials within a second or two of the terminated connection, every wake is signalled after
    // that LISTEN, and the installer reads the store.
    await expect.poll(() => installedVersion(), { timeout: 15_000 }).toBe(builtSchemaVersion);
    expect(await servesAddedType()).toBe(true);

    // The tab loses its socket to that switch, reads the installed schema and connects again
    // with it. The second socket is the one that proves the reconnect.
    await expect
      .poll(sockets, {
        message:
          "the tab opened no second document socket for this document, so the switch closed " +
          "none: the server this ran against either holds no socket registry " +
          "(docs.Service.CloseDocumentSockets) or ran the switch with that registry empty - its " +
          "log says which, on every switch",
        timeout: 15_000,
      })
      .toBeGreaterThanOrEqual(2);
    await expect(connectedDot(page)).toHaveText("connected");

    // The person types again in the same tab, and the text is stored.
    await typeAtEnd(page, "Typed after the switch.");
    await expect(documentEditor(page)).toContainText("Typed after the switch.");
    await expect
      .poll(async () => (await getArtifactText(artifact)).markdown, { timeout: 30_000 })
      .toContain("Typed after the switch.");

    // No line asks the person to reload while the issue is open: the admission the new socket
    // got is a writable one, because it presented the version the server installed.
    await expect(page.getByText("Reload to edit.")).toHaveCount(0);
    await page.screenshot({
      fullPage: true,
      path: testInfo.outputPath("supertag-change-open-editor.png"),
    });

    // The typing is written as a version only once the room settles, a short delay after the
    // last keystroke, so the read waits for that settlement; both lines are asserted on the one
    // read that held the second.
    let stored = "";
    await expect
      .poll(
        async () => {
          stored = await newestStoredMarkdown(artifact);
          return stored;
        },
        { timeout: 30_000 }
      )
      .toContain("Typed after the switch.");
    expect(stored).toContain("Typed before the switch.");

    // The switch needed no restart: every process of the run is the one that started it.
    expect(await processStartTimes()).toBe(startedBefore);

    // What the process now serves takes the write the transition refused.
    const taken = await writeThroughTheTab(
      page,
      key,
      "after-the-switch.md",
      documentWithAddedType("Written once the process had the new schema.")
    );
    expect(taken.status, taken.body).toBe(201);
  } finally {
    await alice.close();
  }
});

/**
 * The seed of the person page: four comments on one issue, and a fifth no reader but its author
 * may see.
 *
 * Two of the store's facts a browser run does not reach by itself, and each is written here as
 * the writer that owns it writes it in a deployment:
 *
 *   - the `delivered_to` tuple of a delivery. Its one writer is the events publisher
 *     (`outbox.recordBatch`), which publishes through NATS, and a browser run has none unless
 *     its caller named one in `NATS_URLS`, which `e2e/run-server.sh` passes through. So the
 *     publisher is given its chance first and the tuple is written here only where it wrote
 *     none. The attempt itself is the server's own either way: the mention is delivered through
 *     the fake Envoy and its row reads `sent`, which is the attempt a delivery records.
 *   - the `alias_of` tuple that makes a session a person's. One credential leaves it - a token
 *     the secrets broker scoped to a person, which proves the session and the person together
 *     and presents a DPoP proof on every request - and no browser can present one, which is why
 *     `e2e/acceptance/graph-store.sh` carries that row to a Go test as well.
 *
 * The fifth comment is written author-private the same way: no route offers that visibility yet,
 * so the node is stored as such a write would store it, and it is delivered to the same session,
 * so only the predicate keeps it off the page.
 */
async function seedThePersonPage(): Promise<{
  issue: string;
  received: string;
}> {
  await setLiveSessions([
    { capabilities: ["steer"], machine_id: "e2e", roles: [], session_id: SESSION, title: "Agent" },
  ]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "A page of a person.",
    title: "Person page",
  });

  await createComment(issue.key, { body: "Alice wrote the first." }, { login: "alice" });
  await createComment(
    issue.key,
    { body: "The session wrote the second." },
    { actor: { id: SESSION, kind: "session" }, as: "agent" }
  );
  const received = await createComment(
    issue.key,
    { body: "Bob wrote the third, for the session.", mentions: [{ target: `session:${SESSION}` }] },
    { login: "bob" }
  );
  await createComment(
    issue.key,
    { body: "Bob wrote the fourth, which reaches neither." },
    { login: "bob" }
  );
  const unseen = await createComment(
    issue.key,
    {
      body: "Bob wrote the fifth, for his own eyes.",
      mentions: [{ target: `session:${SESSION}` }],
    },
    { login: "bob" }
  );
  // The server's own delivery: both mentions reach the live session and their attempts settle
  // `sent`, which is the attempt a delivery's tuple records.
  await expect
    .poll(() =>
      sql(
        `select count(*) from comment_deliveries
         where comment_id in ('${received.id}'::uuid, '${unseen.id}'::uuid) and state = 'sent'`
      )
    )
    .toBe("2");

  // The publisher's own window: where this run has NATS it has written the delivery's tuples
  // itself by now, and where it has not, nothing else will, so they are written here.
  const deliveredTuples = `select count(*) from graph_tuples
     where kind = 'delivered_to' and to_kind = 'session' and to_id = '${SESSION}'
       and from_id in ('${received.id}', '${unseen.id}')`;
  for (let waited = 0; waited < 2_000 && (await sql(deliveredTuples)) !== "2"; waited += 200) {
    // What this spends time on is the delivery publisher's own write of these two
    // `delivered_to` tuples: it runs on the NATS consumer, after the request that sent the
    // mentions had answered, so the read in the loop's own condition can be taken before it.
    // Two seconds is the whole window and it is read every 200 ms, because a run that has NATS
    // writes the tuples in milliseconds and leaves at the first read, while a run without one
    // has nobody to write them at all - which is why the window ends in the insert below
    // rather than in a failure, and why this is no `expect.poll`: a window that expires is the
    // expected half of the branch, not this row's defeat.
    await sleep(200);
  }
  if ((await sql(deliveredTuples)) !== "2") {
    await sql(
      `insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
       select kind, id, 'delivered_to', 'session', '${SESSION}', now(), ${OUTBOX_WRITER}, 0, null, true
       from graph_nodes
       where kind = 'comment' and id in ('${received.id}', '${unseen.id}')
         and not exists (
           select 1 from graph_tuples held
           where held.from_kind = 'comment' and held.from_id = graph_nodes.id
             and held.kind = 'delivered_to' and held.to_id = '${SESSION}')`
    );
  }
  await sql(
    // The session the server proved alice's. No browser credential proves that: it would have to
    // be one the secrets broker scoped to a person, presenting a DPoP proof on every request.
    `insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
     values ('session', '${SESSION}', 'alias_of', 'person', 'alice', now(),
       '{"kind": "session", "id": "${SESSION}"}'::jsonb, 0, null, true)`,
    // The fifth comment as an author-private write stores it: the node reserved for its author,
    // and the base attribute that node row projects.
    `update graph_nodes set visibility = 'author' where kind = 'comment' and id = '${unseen.id}'`,
    `insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
     select 'comment', '${unseen.id}', 'visibility', 'value', 'author', now(), made_by, 0, null, true
     from graph_nodes where kind = 'comment' and id = '${unseen.id}'`
  );
  return {
    issue: issue.key,
    received: received.id,
  };
}

// SLOW(>=2s): five comments written through the API, their deliveries settled, and one page read
// from the navigation with a screenshot.
test("a person page lists what they authored and what reached them", async ({
  browser,
}, testInfo) => {
  const seeded = await seedThePersonPage();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    // From the signed-in login in the navigation rail, which is the one way in to this page.
    await page
      .getByRole("complementary", { name: "Navigation" })
      .getByRole("link", { name: "alice", exact: true })
      .click();
    await expect(page).toHaveURL("/people/alice");
    // The page is headed by that login.
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("alice");

    const rows = page.getByRole("list", { name: "Person nodes" }).getByRole("listitem");
    await expect(rows).toHaveCount(2);
    // Newest first: the received comment, then the authored one.
    await expect(rows.nth(0)).toContainText("Received");
    await expect(rows.nth(0)).toContainText("Bob wrote the third, for the session.");
    await expect(rows.nth(1)).toContainText("Authored");
    await expect(rows.nth(1)).toContainText("Alice wrote the first.");
    // What the store ties to nobody of hers is on no row: the comment that reached neither, the
    // one her session wrote, and the author-private one another person wrote - that one
    // delivered to the same session, so the predicate is what keeps it off.
    await expect(rows.nth(0)).not.toContainText("Authored");
    for (const absent of [
      "Bob wrote the fourth, which reaches neither.",
      "The session wrote the second.",
      "Bob wrote the fifth, for his own eyes.",
    ]) {
      await expect(page.getByText(absent)).toHaveCount(0);
    }
    await page.screenshot({
      fullPage: true,
      path: testInfo.outputPath("person-page.png"),
    });

    // Following the received row's link opens that comment in its thread.
    await rows.nth(0).getByRole("link", { name: "Open" }).click();
    await expect(page).toHaveURL(`/issues/${seeded.issue}/comments/${seeded.received}`);
    await expect(page.getByText("Bob wrote the third, for the session.").first()).toBeVisible();

    // Every row is the store's answer and nothing else's: with the delivery's tuple retracted
    // the received row goes, while the comment and its sent attempt stand where they were. The
    // retraction is a second tuple, as every end of a relation in this store is: nothing but
    // `envoy-dispatch erase` deletes a tuple row.
    await sql(
      `insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
       select from_kind, from_id, kind, to_kind, to_id, now(), who, 0, null, false
       from graph_tuples
       where from_kind = 'comment' and from_id = '${seeded.received}'
         and kind = 'delivered_to' and asserted`
    );
    await page.goto("/people/alice");
    await expect(
      page.getByRole("list", { name: "Person nodes" }).getByRole("listitem")
    ).toHaveCount(1);
    await expect(page.getByText("Alice wrote the first.")).toBeVisible();
    expect(
      await sql(
        `select count(*) from comments c join comment_deliveries d on d.comment_id = c.id
         where c.id = '${seeded.received}'::uuid and d.state = 'sent'`
      )
    ).toBe("1");
    await expect(page.getByText("Bob wrote the third, for the session.")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});
