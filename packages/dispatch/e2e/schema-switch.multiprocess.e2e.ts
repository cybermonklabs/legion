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
import { type Browser, type BrowserContext, expect, type Page, test } from "@playwright/test";

import {
  baseUrl,
  createIssue,
  createIssueArtifact,
  createProject,
  getArtifact,
  getArtifactText,
  getArtifactVersion,
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

/** The document's stored versions as one process reads them, and the markdown of the newest: what
 *  a write was stored as, which the live text a room holds is not. */
async function storedVersions(
  artifact: string,
  target: DispatchProcess
): Promise<{ newest: string; numbers: number[] }> {
  const numbers = ((await getArtifact(artifact, { process: target })).versions ?? []).map(
    (version) => version.number
  );
  const last = numbers.at(-1);
  const newest =
    last === undefined
      ? ""
      : (await getArtifactVersion(artifact, last, { process: target })).markdown;
  return { newest, numbers };
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
  // The document as it is stored before the switch: one version, holding the text it was
  // written with and no block of the type the migration adds.
  const before = await storedVersions(notesID, "a");
  expect(before.numbers, "notes.md is stored as one version before the switch").toHaveLength(1);
  expect(before.newest, "notes.md's one version holds the text it was written with").toContain(
    "Nothing typed yet."
  );
  expect(before.newest, `notes.md's one version holds no :::${ADDED_TYPE} block`).not.toContain(
    `:::${ADDED_TYPE}`
  );

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
    // The migration, applied by psql while the first process runs, with the other process's
    // listener terminated in the same psql session right before the commit: that process hears
    // no notification and holds the schema it has until its hub redials. No Dispatch process
    // performs the migration.
    const names = await listeners();
    expect(names, "both processes name a listener connection").toHaveLength(2);
    await applySupertagMigration({ stopListenerOf: names[1] });

    // Which process is behind, read rather than assumed. The process that is listening installs
    // the new schema within milliseconds of the commit, and the one whose listener was stopped
    // holds the old version until its hub redials, which `pglisten` does not do for a second or
    // more: the two are read until they differ, well inside that second, rather than once, which
    // would race the install of the process that did hear the notification. That read is a poll
    // - both versions every 25 ms until they differ - so nothing here spends a guessed number
    // of milliseconds, and `held` carries the pair the reads settled on into the assertion
    // below, which is what names the two versions when they never do differ.
    let held: Record<DispatchProcess, number> = { a: priorSchemaVersion, b: priorSchemaVersion };
    await expect
      .poll(
        async () => {
          const [versionA, versionB] = await Promise.all([
            installedVersion("a"),
            installedVersion("b"),
          ]);
          held = { a: versionA, b: versionB };
          return new Set([versionA, versionB]).size;
        },
        {
          intervals: [25],
          message:
            "the two processes hold two different installed schema versions: the one whose " +
            "listener the migration stopped still holds the version it had, and the one that " +
            "heard the notification has installed the new one",
          timeout: 600,
        }
      )
      .toBe(2);
    const behind = dispatchProcesses.filter((target) => held[target] === priorSchemaVersion);
    const ahead = dispatchProcesses.filter((target) => held[target] === builtSchemaVersion);
    expect(
      { ahead: ahead.length, behind: behind.length },
      `after the migration process a held ${held.a} and b held ${held.b}, want one of each`
    ).toEqual({ ahead: 1, behind: 1 });
    const second = behind[0] as DispatchProcess;

    // The transition: the second process still holds the old version, so a document holding a
    // block of the new type written through it is refused rather than stored under that version.
    const lost = "A claim the second process has not learned.";
    const refused = await uploadThrough(second, issue.key, "notes.md", documentWithAddedType(lost));
    // Every assertion from here names the process, the version it held at the transition and the
    // write it took, so a failure says which process took what.
    const took = (target: DispatchProcess, write: { body: string; status: number }) =>
      `process ${target}, holding schema version ${held[target]}, answered the write ` +
      `${write.status} ${write.body}`;
    // The stored versions are read before the answer is judged: a stale process that took the
    // write and dropped the block it cannot hold has stored a version without it, and the
    // failure then quotes the block that version lost rather than the status alone.
    const transition = await storedVersions(notesID, second);
    if (refused.status !== 400) {
      expect(
        transition.newest,
        `${took(second, refused)}: the write was taken, and its newest stored version ` +
          `(${transition.numbers.at(-1)}) lost the block ":::${ADDED_TYPE}{#c1} ${lost}"`
      ).toContain(`:::${ADDED_TYPE}`);
      expect(
        transition.newest,
        `${took(second, refused)}: the newest stored version lost "${lost}"`
      ).toContain(lost);
    }
    expect(refused.status, took(second, refused)).toBe(400);
    expect(refused.body, took(second, refused)).toContain(
      `unknown typed block \\"${ADDED_TYPE}\\"`
    );
    // The stored version is the one before, with no block dropped from it: the refused write
    // stored nothing.
    expect(transition.numbers, `${took(second, refused)}; notes.md's stored versions`).toEqual(
      before.numbers
    );
    expect(transition.newest, took(second, refused)).toContain("Nothing typed yet.");
    expect(transition.newest, took(second, refused)).not.toContain(`:::${ADDED_TYPE}`);

    // The second process handles its notification once its listener is back: `pglisten.Hub`
    // redials, every wake is signalled after that LISTEN, and the installer reads the store. The
    // installed version then reads equal on both.
    for (const target of dispatchProcesses) {
      await expect
        .poll(() => installedVersion(target), {
          message: `${took(target, refused)}; it never installed version ${builtSchemaVersion}`,
          timeout: 30_000,
        })
        .toBe(builtSchemaVersion);
    }

    // The same document written again through the second process stores the new block, in a
    // version of its own.
    const kept = "A claim the second process now holds.";
    const taken = await uploadThrough(second, issue.key, "notes.md", documentWithAddedType(kept));
    expect(taken.status, took(second, taken)).toBe(201);
    const after = await storedVersions(notesID, second);
    expect(
      after.numbers.at(-1),
      `${took(second, taken)}; notes.md's newest stored version, against ${before.numbers.join(", ")} before`
    ).toBeGreaterThan(before.numbers.at(-1) ?? 0);
    expect(after.newest, `${took(second, taken)}; the newest stored version`).toContain(
      `:::${ADDED_TYPE}`
    );
    expect(after.newest, `${took(second, taken)}; the newest stored version`).toContain(kept);

    // Neither process restarted.
    expect(await processStartTimes(), took(second, taken)).toBe(startedBefore);

    // The editor socket opened before the change writes again, with no reload: the tab lost its
    // socket to the switch, read the installed schema and connected with it.
    const page = pages[second] as Page;
    await expect(connectedDot(page), took(second, taken)).toHaveText("connected");
    await typeAndStore(page, artifact, `Typed on process ${second} after the switch.`);
    await expect(page.getByText("Reload to edit."), took(second, taken)).toHaveCount(0);
  } finally {
    for (const context of Object.values(contexts)) {
      await context.close();
    }
  }
});
