import { expect, test } from "@playwright/test";

import type { Actor } from "../web/src/api/types";
import { createIssue, createProject, patchIssue } from "./api";
import { connectedDot, countDocumentSockets, documentEditor, reconnectedSockets } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// The server admits a document socket read-only for a closed issue exactly as it does for a page
// whose schema the live fleet does not admit, so the admission alone never says whether a reload
// would help: the close state the page holds now does. This row drives the one case where the two
// disagree - an admission that came while the page still held the issue closed, on a page that
// then learns the issue is open - and the page must name the reload that makes the document
// editable again.

test.beforeEach(async () => {
  await resetDatabase();
});

/** The session that reopens the issue behind the page's back. */
const reopener = {
  actor: { id: "e2e-doc-reload", kind: "session" } as Actor,
  as: "agent" as const,
};

// SLOW(>=2s): the property is what one page holds across three admissions, so the row pays one
// issue page load with its document editor and its socket, then a close, a reopen made behind
// the page's back, and the reconnect each of them drops - all on the one page, since a reload
// would answer the question the row asks.
// @webkit @firefox: the socket the engine reconnects and the paint that follows it, which decide
// whether a read-only admission landing before the issue read leaves a reader with no way out.
test("a reconnect admitted read-only before the issue read names the reload @webkit @firefox", async ({
  browser,
}) => {
  await createProject({ key: "DOC", name: "Doc reload" });
  const issue = await createIssue({
    project: "DOC",
    spec: "## Plan\n\nContext.\n",
    title: "Reload to edit",
  });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const sockets = countDocumentSockets(page, issue.primary_artifact_id);
    // A creatable version above every live process's schema version: every socket this page opens
    // is admitted read-only, whatever the issue's state. The page presents its compiled manifest
    // on each document socket - `schema_version`, what it reads, and `schema_creatable`, what it
    // may create (web/src/features/doc/connection.ts) - and the server admits a page writable only
    // while every live process reads what that page creates (docs.schemaFleet.admits). So this
    // raises the creatable version one above the version the page reads, which is this server's
    // own. Raising the version alone leaves the page writable, since the fleet still reads what it
    // creates (TestSchemaAdmissionFollowsTheLiveFleet, "a page one version newer that creates only
    // what every live process reads"). Rewriting `GET /api/v1/schema/blocks`, which is how this
    // row made the page read-only while the page read its schema from the server, changes nothing
    // now: the page reads no schema from the server (web/src/features/doc/schema.ts), so the route
    // is gone from here. The raise is made where the page builds the socket URL, since Playwright
    // 1.64's `routeWebSocket` reaches the server only at the URL the page opened
    // (`connectToServer()` takes none).
    await page.addInitScript(() => {
      const Native = window.WebSocket;
      window.WebSocket = class extends Native {
        constructor(url: string | URL, protocols?: string | string[]) {
          const raised = new URL(String(url), window.location.href);
          if (raised.pathname.startsWith("/ws/doc/")) {
            const version = Number(raised.searchParams.get("schema_version"));
            raised.searchParams.set("schema_creatable", String(version + 1));
          }
          super(raised.toString(), protocols);
        }
      };
    });
    // The page's first read of the issue that would tell it the issue is open again, held in the
    // handler: while it is held the page still holds the issue closed, which is the state the
    // reopen's reconnect is admitted under. Any read before the reopen lands goes through, so the
    // hold is spent on the one read that matters.
    const reading = Promise.withResolvers<void>();
    const release = Promise.withResolvers<void>();
    let hold = false;
    await page.route(`**/api/v1/issues/${issue.key}`, async (route) => {
      if (route.request().method() !== "GET" || !hold) return route.fallback();
      const response = await route.fetch();
      const read = (await response.json()) as { closed_at: string | null };
      if (read.closed_at !== null) {
        await route.fulfill({ json: read, response });
        return;
      }
      hold = false;
      reading.resolve();
      await release.promise;
      await route.fulfill({ json: read, response });
    });
    await page.clock.install();
    await page.goto(`/issues/${issue.key}`);

    // Open, connected and read-only because of the schema alone: a reload opens a socket the
    // server admits on the issue as it stands, so the page names it.
    await expect(documentEditor(page)).toContainText("Context.");
    await expect(connectedDot(page)).toBeVisible();
    await expect(documentEditor(page)).toHaveAttribute("contenteditable", "false");
    await expect(page.getByText("Reload to edit.")).toBeVisible();

    // The close drops the document's socket; the reconnect is admitted read-only again, and the
    // closed issue shows its own line instead, since no reload makes a closed issue editable.
    await page.getByRole("button", { name: "Close issue" }).click();
    await expect(page.getByRole("button", { name: "Reopen" })).toBeVisible();
    await expect(page.getByText("This issue is closed. Its document is read-only.")).toBeVisible();
    await reconnectedSockets(page, sockets, 2);
    await expect(connectedDot(page)).toBeVisible();
    await expect(page.getByText("Reload to edit.")).toHaveCount(0);

    // Someone else reopens the issue. The page's read of that is held, so the socket the reopen
    // dropped reconnects - and is admitted read-only once more - while the page still holds the
    // issue closed. Only then does the page learn the issue is open.
    hold = true;
    await patchIssue(issue.key, { status: "backlog" }, reopener);
    await reading.promise;
    await reconnectedSockets(page, sockets, 3);
    release.resolve();

    await expect(page.getByRole("button", { name: "Close issue" })).toBeVisible();
    await expect(page.getByText("This issue is closed. Its document is read-only.")).toHaveCount(0);
    await expect(connectedDot(page)).toBeVisible();
    await expect(documentEditor(page)).toHaveAttribute("contenteditable", "false");
    await expect(page.getByText("Reload to edit.")).toBeVisible();
  } finally {
    await alice.close();
  }
});
