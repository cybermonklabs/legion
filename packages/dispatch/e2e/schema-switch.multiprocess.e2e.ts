/**
 * Two server processes over one database must agree on the installed block schema, and the one
 * that has not read a supertag migration yet must refuse what its schema cannot hold rather than
 * take it and drop the block.
 *
 * Only the two-process lane runs this (`bun run e2e:multiprocess`): a second browser connection is
 * not a second process, so every read below is pinned to a process of its own
 * (`e2e/harness-processes.ts`). The migration itself, and why a browser run moves the version up
 * to the binary's own rather than past it, is `e2e/block-schema.ts`.
 *
 * Which of the two is held back is discovered, not assumed. A process names its listener
 * connection in `pg_stat_activity` by its own id (`docs.ListenerApplicationPrefix`), and no answer
 * of the server ties that id to the pin a request carries, so this spec stops one listener and
 * then reads which process is behind. Both processes hold an editor socket opened before the
 * change, so whichever turns out to be the second one has the socket the last step needs.
 */
import { setTimeout as sleep } from "node:timers/promises";

import { type Browser, type BrowserContext, expect, type Page, test } from "@playwright/test";

import {
  baseUrl,
  createIssue,
  createIssueArtifact,
  createProject,
  getArtifactText,
  userHeaders,
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
  storeSchemaBeforeTheChange,
} from "./block-schema";
import { connectedDot, documentEditor, typeAtEnd } from "./editor";
import {
  type DispatchProcess,
  dispatchProcesses,
  pinnedTo,
  processPinCookie,
} from "./harness-processes";
import { resetDatabase } from "./seed";
import { signIn } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
  // Both ends of the restore: a row that failed before its own must not leave the next one, or
  // the lane's other specs, on another schema.
  await restoreStoreSchema();
});

test.afterEach(async () => {
  await restoreStoreSchema();
  for (const target of dispatchProcesses) {
    await expect.poll(() => installedVersion(target), { timeout: 30_000 }).toBe(builtSchemaVersion);
  }
});

/** A browser context whose every request - websocket upgrades included - reaches one process. */
async function contextOn(browser: Browser, target: DispatchProcess): Promise<BrowserContext> {
  const context = await browser.newContext();
  await context.addCookies([{ name: processPinCookie, url: baseUrl, value: target }]);
  await signIn(context, "alice");
  return context;
}

/** Uploads `markdown` as the issue's document `name` through one process, as the person alice.
 *  It answers the status and the body, so a refusal is read rather than thrown. */
async function uploadThrough(
  target: DispatchProcess,
  issueKey: string,
  name: string,
  markdown: string
): Promise<{ body: string; status: number }> {
  const response = await fetch(new URL(`/api/v1/issues/${issueKey}/artifacts`, baseUrl), {
    body: JSON.stringify({ content: markdown, name }),
    headers: {
      ...(await userHeaders("alice")),
      ...pinnedTo(target),
      "Content-Type": "application/json",
    },
    method: "POST",
  });
  return { body: await response.text(), status: response.status };
}

/** Types in one tab's editor and waits until the server it is pinned to holds the text. */
async function typeAndStore(page: Page, artifact: string, text: string): Promise<void> {
  await typeAtEnd(page, text);
  await expect(documentEditor(page)).toContainText(text);
  await expect
    .poll(async () => (await getArtifactText(artifact)).markdown, { timeout: 30_000 })
    .toContain(text);
}

// SLOW(>=2s): two processes, two editor tabs, a migration one of them misses, and the catch-up.
test("two processes agree on the installed schema", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "The first line.",
    title: "Two processes and one schema",
  });
  const artifact = issue.primary_artifact_id;
  if (artifact === undefined || artifact === null) {
    throw new Error(`the issue ${issue.key} has no spec document`);
  }
  // The document the refused write is a version of: ordinary markdown, which both schemas hold.
  const notes = await createIssueArtifact(
    issue.key,
    { content: "# Notes\n\nNothing typed yet.\n", name: "notes.md" },
    { login: "alice", process: "a" }
  );
  const notesID = notes.artifact.id;

  await storeSchemaBeforeTheChange();
  for (const target of dispatchProcesses) {
    await expect.poll(() => installedVersion(target), { timeout: 30_000 }).toBe(priorSchemaVersion);
  }
  const startedBefore = await processStartTimes();
  expect(startedBefore.split("\n")).toHaveLength(2);

  // An editor socket on each process, open and connected before the change.
  const contexts: Record<string, BrowserContext> = {};
  const pages: Record<string, Page> = {};
  for (const target of dispatchProcesses) {
    contexts[target] = await contextOn(browser, target);
    const page = await contexts[target].newPage();
    pages[target] = page;
    await page.goto(`/issues/${issue.key}/spec`);
    await expect(documentEditor(page)).toContainText("The first line.");
    await expect(connectedDot(page)).toHaveText("connected");
  }

  try {
    // The migration, with one process's listener terminated in the same psql session right before
    // the commit: that process hears no notification and holds the schema it has until its hub
    // redials.
    const names = await listeners();
    expect(names, "both processes name a listener connection").toHaveLength(2);
    await applySupertagMigration({ stopListenerOf: names[1] });

    // Which process is behind, read rather than assumed. The process that is listening installs
    // the new schema within milliseconds of the commit, and the one whose listener was stopped
    // holds the old version until its hub redials, which `pglisten` does not do for a second or
    // more: the two are read until they differ, well inside that second, rather than once, which
    // would race the install of the process that did hear the notification.
    let held: Record<DispatchProcess, number> = { a: priorSchemaVersion, b: priorSchemaVersion };
    for (let waited = 0; waited < 600 && held.a === held.b; waited += 25) {
      const [versionA, versionB] = await Promise.all([
        installedVersion("a"),
        installedVersion("b"),
      ]);
      held = { a: versionA, b: versionB };
      if (held.a === held.b) {
        await sleep(25);
      }
    }
    const behind = dispatchProcesses.filter((target) => held[target] === priorSchemaVersion);
    const ahead = dispatchProcesses.filter((target) => held[target] === builtSchemaVersion);
    expect(
      { ahead: ahead.length, behind: behind.length },
      `after the migration process a held ${held.a} and b held ${held.b}, want one of each`
    ).toEqual({ ahead: 1, behind: 1 });
    const second = behind[0] as DispatchProcess;

    // The transition: the second process still holds the old version, so a document holding a
    // block of the new type written through it is refused rather than stored under that version.
    const refused = await uploadThrough(
      second,
      issue.key,
      "notes.md",
      documentWithAddedType("A claim the second process has not learned.")
    );
    expect(
      refused.status,
      `process ${second}, holding version ${held[second]}, answered ${refused.body}`
    ).toBe(400);
    expect(refused.body).toContain(`unknown typed block \\"${ADDED_TYPE}\\"`);
    // The stored version is the one before, with no block dropped from it.
    const stored = await getArtifactText(notesID, { process: second });
    expect(stored.markdown).toContain("Nothing typed yet.");
    expect(stored.markdown).not.toContain(`:::${ADDED_TYPE}`);

    // The second process handles its notification once its listener is back: `pglisten.Hub`
    // redials, every wake is signalled after that LISTEN, and the installer reads the store. The
    // installed version then reads equal on both.
    for (const target of dispatchProcesses) {
      await expect
        .poll(() => installedVersion(target), { timeout: 30_000 })
        .toBe(builtSchemaVersion);
    }

    // The same document written again through the second process stores the new block.
    const taken = await uploadThrough(
      second,
      issue.key,
      "notes.md",
      documentWithAddedType("A claim the second process now holds.")
    );
    expect(taken.status, taken.body).toBe(201);
    const after = await getArtifactText(notesID, { process: second });
    expect(after.markdown).toContain(`:::${ADDED_TYPE}`);
    expect(after.markdown).toContain("A claim the second process now holds.");

    // Neither process restarted.
    expect(await processStartTimes()).toBe(startedBefore);

    // The editor socket opened before the change writes again, with no reload: the tab lost its
    // socket to the switch, read the installed schema and connected with it.
    const page = pages[second] as Page;
    await expect(connectedDot(page)).toHaveText("connected");
    await typeAndStore(page, artifact, `Typed on process ${second} after the switch.`);
    await expect(page.getByText("Reload to edit.")).toHaveCount(0);
  } finally {
    for (const context of Object.values(contexts)) {
      await context.close();
    }
  }
});
