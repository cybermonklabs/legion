/** The server must decide which document fragments share a room, whose updates it keeps, which
 *  clients see each edit and cursor, and when an open socket reconnects after a schema change.
 *  An interchangeable stub could pass without the kept editor ever speaking to the real room. */
import { setTimeout as delay } from "node:timers/promises";
import { MessageType } from "@hocuspocus/provider";
import type { Locator, Page } from "@playwright/test";
import { createDecoder, readVarString, readVarUint, readVarUint8Array } from "lib0/decoding";
import { messageYjsSyncStep2, messageYjsUpdate } from "y-protocols/sync";
import * as Y from "yjs";

import { presetPath } from "../web/src/features/issue-page/issue-address";
import { createIssue, createProject, editArtifact, getArtifact, getArtifactText } from "./api";
import {
  BLOCK_EDITOR_READY_MARK,
  BLOCK_EDITOR_START_MARK,
  forbiddenReadsIn,
  isRoomSocket,
  readMark,
  waitForMark,
} from "./block-editor";
import { applySupertagMigration, restoreStoreSchema, servesAddedType } from "./block-schema";
import {
  composer,
  connectedDot,
  documentEditor,
  documentTransport,
  openDocumentSockets,
  openSpecAndAwaitHeadingIds,
  selectEditorText,
  typeAtEnd,
} from "./editor";
import {
  EDITOR_BOX_SELECTOR,
  HEADER_SELECTORS,
  LOAD_ROW_SELECTOR,
  VIEW_ROW_SELECTOR,
} from "./issue-page-load";
import { withIssuePage } from "./issue-page-option";
import { editNoticesSent } from "./issue-page-t-rows";
import { type LargestShapeSeed, seedLargestShape } from "./issue-page-t-scale";
import { heldAnswer, outlineAnswerOf, seedEventsLanded } from "./issue-page-t-tree";
import {
  installInstruments,
  recordOf,
  waitForLoadEnd,
  watchLoadRequests,
} from "./load-instruments";
import { type Browser, expect, test } from "./test";
import { watchApiRequests } from "./traffic";
import { asUser } from "./users";

const FIRST = "r1";
const SECOND = "r2";
const CHECK = "c1";
const WORDS = "[data-node-words]";
const FIRST_ROOM_TIMEOUT = 20_000;
const ROW_TIMEOUT = 240_000;

function syncPayload(frame: string | Buffer, step: number): Uint8Array | undefined {
  if (typeof frame === "string") return;
  const decoder = createDecoder(frame);
  readVarString(decoder);
  if (readVarUint(decoder) === MessageType.Sync && readVarUint(decoder) === step) {
    return readVarUint8Array(decoder);
  }
}

/** The first server SyncStep2 on each real connection, including a reconnect's. */
function roomFrames(page: Page) {
  const sockets: { frame?: Buffer; closed: boolean; start: number; path: string }[] = [];
  page.on("websocket", (socket) => {
    if (!isRoomSocket(socket.url())) return;
    const entry: (typeof sockets)[number] = {
      closed: false,
      start: Date.now(),
      path: new URL(socket.url()).pathname,
    };
    sockets.push(entry);
    socket.on("framereceived", ({ payload }) => {
      if (
        typeof payload !== "string" &&
        entry.frame === undefined &&
        syncPayload(payload, messageYjsSyncStep2)
      ) {
        entry.frame = payload;
      }
    });
    socket.on("close", () => {
      entry.closed = true;
    });
  });
  return {
    sockets,
    async frame(at = 0) {
      await expect
        .poll(() => sockets[at]?.frame !== undefined, {
          message: `document socket ${at + 1} sent no first SyncStep2`,
          timeout: FIRST_ROOM_TIMEOUT,
        })
        .toBe(true);
      const result = sockets[at]?.frame;
      if (result === undefined) throw new Error("SyncStep2 disappeared from the socket recorder");
      return result;
    },
  };
}

async function inReader<T>(
  browser: Browser,
  login: string,
  outline: boolean,
  use: (page: Page) => Promise<T>
) {
  const context = await asUser(browser, login);
  try {
    if (outline) await withIssuePage(context, login);
    return await use(await context.newPage());
  } finally {
    await context.close();
  }
}

async function openWholeSpec(page: Page, key: string) {
  await page.goto(`/issues/${key}/spec`);
  await expect(connectedDot(page)).toHaveText("connected");
}

async function snapshot(browser: Browser, key: string): Promise<Y.Doc> {
  return await inReader(browser, "bob", false, async (page) => {
    const frames = roomFrames(page);
    await openWholeSpec(page, key);
    const payload = syncPayload(await frames.frame(), messageYjsSyncStep2);
    if (payload === undefined) throw new Error("Room snapshot had no SyncStep2 payload");
    const doc = new Y.Doc();
    Y.applyUpdate(doc, payload);
    return doc;
  });
}

function printCapture(name: string, frame: Buffer) {
  console.log(`SYNC-CAPTURE ${name} ${frame.toString("base64")}`);
}

// SLOW(>=2s): three real rooms, two signed-in writers, a deletion, and a live reconnect.
test("capture the room's first sync frames", async ({ browser }) => {
  test.skip(
    process.env.BLOCK_ROOM_CAPTURE !== "1",
    "only the real room fixture Job captures frames"
  );
  test.setTimeout(ROW_TIMEOUT);
  await createProject({ key: "SYNC", name: "Room sync fixtures" });
  const empty = await createIssue({ project: "SYNC", spec: "", title: "Empty room" });
  const single = await createIssue({
    project: "SYNC",
    spec: "One writer started here.\n",
    title: "One writer",
  });
  const multiple = await createIssue({
    project: "SYNC",
    title: "Two writers and a deletion",
    spec: ":::requirement{#keep}\nKeep this text.\n:::\n\n:::requirement{#remove}\nRemove this block.\n:::\n",
  });
  await inReader(browser, "alice", false, async (page) => {
    const frames = roomFrames(page);
    await openWholeSpec(page, empty.key);
    printCapture("empty", await frames.frame());
  });
  await inReader(browser, "alice", false, async (page) => {
    const frames = roomFrames(page);
    await openWholeSpec(page, single.key);
    await expect(documentEditor(page)).toContainText("One writer started here.");
    printCapture("single", await frames.frame());
  });
  await inReader(browser, "alice", false, async (alice) => {
    await inReader(browser, "bob", false, async (bob) => {
      await Promise.all([openWholeSpec(alice, multiple.key), openWholeSpec(bob, multiple.key)]);
      await typeAtEnd(alice, "Alice added a line.");
      await expect(documentEditor(bob)).toContainText("Alice added a line.");
      await typeAtEnd(bob, "Bob added a line.");
      await expect(documentEditor(alice)).toContainText("Bob added a line.");
      await editArtifact(multiple.primary_artifact_id, {
        ops: [{ op: "delete", block: "remove" }],
      });
      await expect(documentEditor(alice)).not.toContainText("Remove this block.");
      await expect(documentEditor(bob)).not.toContainText("Remove this block.");
    });
  });
  await inReader(browser, "alice", false, async (page) => {
    const frames = roomFrames(page);
    const transport = await documentTransport(page);
    await openWholeSpec(page, multiple.key);
    const first = await frames.frame();
    const update = syncPayload(first, messageYjsSyncStep2);
    if (update === undefined) throw new Error("Expected the full room update");
    const { structs, ds } = Y.decodeUpdate(update);
    expect(new Set(structs.map((struct) => struct.id.client)).size).toBeGreaterThan(1);
    expect(ds.clients.size, "server frame holds the deleted block's delete set").toBeGreaterThan(0);
    printCapture("multiple", first);
    if (structs.some((struct) => struct instanceof Y.GC)) printCapture("gc", first);
    else console.log("SYNC-CAPTURE gc none");
    await transport.sever();
    printCapture("reconnect", await frames.frame(1));
  });
});

function rowOf(page: Page, id: string): Locator {
  return page.locator(`[data-issue-body] [data-node-row][data-node-id$="#b-${id}"]`).first();
}

function rowEditor(row: Locator): Locator {
  return row.locator(`${EDITOR_BOX_SELECTOR} [contenteditable="true"]`);
}

async function openOutline(page: Page, key: string, block?: string) {
  await page.goto(`${presetPath(key, "conversation")}${block === undefined ? "" : `#b-${block}`}`, {
    waitUntil: "commit",
  });
}

async function ready(page: Page) {
  await waitForMark(page, BLOCK_EDITOR_READY_MARK);
}

async function enter(page: Page, row: Locator) {
  await row.focus();
  await page.keyboard.press("Enter");
  await expect(rowEditor(row), "the row's block editor has the caret").toBeFocused();
}

async function leaveEditor(page: Page, row: Locator) {
  await page.keyboard.press("Escape");
  await expect(row.locator(EDITOR_BOX_SELECTOR)).toHaveCount(0);
  await expect(row).toBeFocused();
}

async function switchView(page: Page, key: string, title: "Spec" | "Conversation") {
  const presets = page.getByTestId("issue-presets");
  const desktop = presets.getByRole("button", { name: new RegExp(`^${title}\\b`) });
  if (await desktop.isVisible()) {
    await desktop.click();
  } else {
    await page.locator('[data-testid="issue-presets"] button[aria-haspopup="menu"]').click();
    await page.getByRole("menuitemradio", { name: new RegExp(`^${title}\\b`) }).click();
  }
  const path = presetPath(key, title === "Spec" ? "spec" : "conversation");
  await expect.poll(() => new URL(page.url()).pathname).toBe(path);
}

/** The caret offset into the row's editable text, from the beginning of the first paragraph. */
async function caretIn(editor: Locator): Promise<number | null> {
  return await editor.evaluate((element) => {
    const selection = document.getSelection();
    if (
      !selection?.isCollapsed ||
      !selection.anchorNode ||
      !element.contains(selection.anchorNode)
    ) {
      return null;
    }
    const range = document.createRange();
    range.selectNodeContents(element.querySelector("p") ?? element);
    range.setEnd(selection.anchorNode, selection.anchorOffset);
    return range.toString().length;
  });
}

/** The seam between two characters of the row's first line, not the center of the whole row. */
async function clickAt(page: Page, words: Locator, offset: number) {
  await words.scrollIntoViewIfNeeded();
  const point = await words.evaluate((element, at) => {
    const walk = document.createTreeWalker(element, NodeFilter.SHOW_TEXT);
    const text = walk.nextNode();
    if (!text || (text.textContent?.length ?? 0) <= at)
      throw new Error("No characters at press offset");
    const range = document.createRange();
    range.setStart(text, at);
    range.collapse(true);
    const box = range.getBoundingClientRect();
    return { x: box.x, y: box.y + box.height / 2 };
  }, offset);
  await page.mouse.click(point.x, point.y);
}

async function inSeed<T>(
  browser: Browser,
  use: (seed: LargestShapeSeed, page: Page) => Promise<T>
) {
  const seed = await seedLargestShape();
  return await inReader(browser, "alice", true, (page) => use(seed, page));
}

function firstRow(page: Page) {
  return rowOf(page, FIRST);
}

// SLOW(>=2s): the real seed and three cold loads, one quiet and two with input at the first rows.
test("the block editor opens one room and reads nothing @phone", async ({ browser }) => {
  test.setTimeout(ROW_TIMEOUT);
  const seed = await seedLargestShape();
  // Let seed writes finish before measuring the page's own request window.
  await inReader(browser, "alice", false, async (page) => {
    const traffic = watchApiRequests(page);
    await seedEventsLanded(page);
    await editNoticesSent(seed.key, traffic);
  });
  for (const act of ["after-ready", "enter", "click"] as const) {
    await inReader(browser, "alice", true, async (page) => {
      const tree = outlineAnswerOf(page, "conversation");
      const api = watchApiRequests(page);
      const load = watchLoadRequests(page);
      const frames = roomFrames(page);
      await installInstruments(page, {
        boxes: "none",
        header: HEADER_SELECTORS,
        named: null,
        rows: LOAD_ROW_SELECTOR,
        viewRows: VIEW_ROW_SELECTOR,
      });
      await openOutline(page, seed.key, FIRST);
      const row = firstRow(page);
      if (act === "after-ready") {
        const end = await waitForLoadEnd(page, load);
        const { timeOrigin } = await recordOf(page);
        api.reset();
        await ready(page);
        await enter(page, row);
        expect(forbiddenReadsIn(api.requests.map(({ path }) => path))).toEqual([]);
        expect(frames.sockets[0]?.start, "room opened after the load ended").toBeGreaterThan(
          timeOrigin + end
        );
      } else {
        await expect(row).toBeVisible();
        expect(
          await readMark(page, BLOCK_EDITOR_START_MARK),
          "work had not started before the early act"
        ).toBeNull();
        if (act === "enter") {
          await row.focus();
          await page.keyboard.press("Enter");
        } else {
          await clickAt(page, row.locator(WORDS), 2);
        }
        await waitForMark(page, BLOCK_EDITOR_START_MARK);
        await frames.frame();
        await ready(page);
        await expect(rowEditor(row)).toBeFocused();
      }
      const { converted } = await heldAnswer(tree, "the seed's Conversation tree");
      console.log(`block-editor ${act} converted=${converted}`);
      expect(converted, "the seed's documents must share one converted room").toBe(true);
      await leaveEditor(page, row);
      const second = rowOf(page, SECOND);
      await enter(page, second);
      await leaveEditor(page, second);
      if (act === "click") {
        await switchView(page, seed.key, "Spec");
        await switchView(page, seed.key, "Conversation");
      }
      expect(frames.sockets.map(({ path, closed }) => ({ path, closed }))).toEqual([
        { path: `/ws/doc/${seed.specArtifactId}`, closed: false },
      ]);
    });
  }
});

// SLOW(>=2s): the seeded outline, a prepared live room, and frames on both sides of the first bind.
test("the block editor is drawn where the row's text stood @phone", async ({ browser }) => {
  test.setTimeout(ROW_TIMEOUT);
  await inSeed(browser, async ({ key }, page) => {
    await openOutline(page, key, FIRST);
    await ready(page);
    const row = firstRow(page);
    const neighbor = rowOf(page, SECOND);
    const before = await row.locator(WORDS).boundingBox();
    const neighborBefore = await neighbor.boundingBox();
    expect(before).not.toBeNull();
    expect(neighborBefore).not.toBeNull();
    await row.evaluate((element) => {
      const seen: string[] = [];
      Object.assign(element, { blockEditorFrames: seen });
      let firstKey = false;
      const typed = (event: Event) => {
        if (
          !(event.target instanceof Element) ||
          event.target.closest("[data-editor-box] [contenteditable]") === null
        )
          return;
        firstKey = true;
        element.removeEventListener("keydown", typed, true);
      };
      element.addEventListener("keydown", typed, true);
      const frame = () => {
        if (firstKey || !element.isConnected) return;
        const focused = element.querySelector("[data-editor-box] [contenteditable]:focus");
        if (focused) seen.push(focused.textContent ?? "");
        requestAnimationFrame(frame);
      };
      requestAnimationFrame(frame);
    });
    await enter(page, row);
    const drawn = await row.locator(EDITOR_BOX_SELECTOR).boundingBox();
    for (const axis of ["x", "y", "width", "height"] as const) {
      expect(drawn?.[axis], `edited row's ${axis}`).toBeCloseTo(before?.[axis] ?? -1, 0);
    }
    expect(await neighbor.boundingBox()).toEqual(neighborBefore);
    const frames = () =>
      row.evaluate((element) => Reflect.get(element, "blockEditorFrames") as string[]);
    await expect.poll(async () => (await frames()).length).toBeGreaterThanOrEqual(4);
    await page.keyboard.press("x");
    expect(
      new Set(await frames()).size,
      "the focused block's content does not change in the first frames"
    ).toBe(1);
  });
});

// SLOW(>=2s): two signed-in contexts on the seed and a third writer's actual room update.
test("the block editor shows two browsers each other's text and cursor live @phone", async ({
  browser,
}) => {
  test.setTimeout(ROW_TIMEOUT);
  await inSeed(browser, async (seed, alice) => {
    await inReader(browser, "bob", true, async (bob) => {
      await Promise.all([openOutline(alice, seed.key, FIRST), openOutline(bob, seed.key, FIRST)]);
      await Promise.all([ready(alice), ready(bob)]);
      await enter(bob, firstRow(bob));
      await enter(alice, firstRow(alice));
      // A peer's caret is not drawn where it stands on the focused editor's own caret
      // (`@legion/proof-editor`'s `collab-cursor-plugin.ts`), and bob's caret at the block's start
      // would move along with each letter alice types there, so bob's caret goes to the end.
      await bob.keyboard.press("End");
      await alice.keyboard.type("sharedword ");
      await expect(rowEditor(firstRow(bob))).toContainText("sharedword", { timeout: 2_000 });
      await expect(rowEditor(firstRow(bob)).locator(".proof-collab-cursor__label")).toBeVisible();
      await bob.keyboard.type("replyword ");
      await expect(rowEditor(firstRow(alice))).toContainText("replyword", { timeout: 2_000 });
      const second = await rowOf(alice, SECOND).locator(WORDS).innerText();
      await editArtifact(seed.specArtifactId, {
        ops: [{ op: "replace", find: second.trim(), with: "A third writer's words." }],
      });
      await expect(rowOf(alice, SECOND)).toContainText("A third writer's words.");
    });
  });
});

// SLOW(>=2s): three binds and the real room acknowledges an edit of each block before undo.
test("the block editor's undo takes only the block's edits @phone", async ({ browser }) => {
  test.setTimeout(ROW_TIMEOUT);
  await inSeed(browser, async ({ key, specArtifactId }, page) => {
    await openOutline(page, key, FIRST);
    await ready(page);
    const a = firstRow(page);
    const b = rowOf(page, SECOND);
    await enter(page, a);
    await page.keyboard.type("keepfirst ");
    await expect
      .poll(async () => (await getArtifactText(specArtifactId)).markdown)
      .toContain("keepfirst");
    await leaveEditor(page, a);
    await enter(page, b);
    await page.keyboard.type("undosecond ");
    await expect
      .poll(async () => (await getArtifactText(specArtifactId)).markdown)
      .toContain("undosecond");
    await page.keyboard.press("ControlOrMeta+z");
    await expect(rowEditor(b)).not.toContainText("undosecond");
    await expect
      .poll(async () => {
        const { markdown } = await getArtifactText(specArtifactId);
        return [markdown.includes("keepfirst"), markdown.includes("undosecond")];
      })
      .toEqual([true, false]);
  });
});

/** What the server sends to a fresh peer, with only the edited block's contents masked. */
function roomWithout(doc: Y.Doc, fragments: readonly string[], editedFragment: string, id: string) {
  let target: Y.XmlElement | undefined;
  function visit(element: Y.XmlFragment, fragment: string) {
    for (const child of element.toArray()) {
      if (!(child instanceof Y.XmlElement)) continue;
      if (fragment === editedFragment && child.getAttribute("blockId") === id) target = child;
      else visit(child, fragment);
    }
  }
  for (const fragment of fragments) visit(doc.getXmlFragment(fragment), fragment);
  if (target === undefined) throw new Error(`the room has no ${editedFragment} block ${id}`);
  const attributes = target.getAttributes();
  target.delete(0, target.length);
  return {
    attributes,
    fragments: fragments.map((name) => doc.getXmlFragment(name).toString()),
    marks: doc.getMap("marks").toJSON(),
  };
}

// SLOW(>=2s): two edits across document fragments, each compared using fresh socket snapshots.
test("the room differs only inside the block after an edit @phone", async ({ browser }) => {
  test.setTimeout(ROW_TIMEOUT);
  await inSeed(browser, async (seed, page) => {
    const artifacts = await Promise.all(
      [seed.specArtifactId, seed.verificationArtifactId, seed.planArtifactId].map((id) =>
        getArtifact(id)
      )
    );
    const fragments = artifacts.map((artifact) => artifact.fragment ?? "prosemirror");
    expect(new Set(fragments).size, "the converted seed holds three fragments").toBe(3);
    // The room's snapshots are read in the whole document's editor (`snapshot`), which writes the
    // id of each heading it derives from the heading's words as it first opens a document whose
    // headings hold none (Milkdown's `syncHeadingIdPlugin`): that write is made, and stored, here,
    // before the first snapshot, so no write of the snapshots' own reader lands between two.
    const heading = /^#+ (.+)$/m.exec((await getArtifactText(seed.specArtifactId)).markdown)?.[1];
    if (heading === undefined) throw new Error("the seed's spec holds no heading");
    await inReader(browser, "bob", false, (bob) =>
      openSpecAndAwaitHeadingIds(bob, seed.key, seed.specArtifactId, heading)
    );
    const sockets = openDocumentSockets(page);
    await openOutline(page, seed.key, FIRST);
    await ready(page);
    for (const [artifact, id, word] of [
      [0, FIRST, "onedoc"],
      [1, CHECK, "otherdoc"],
    ] as const) {
      const r = rowOf(page, id);
      const before = await snapshot(browser, seed.key);
      try {
        const name = fragments[artifact];
        const snapshotBefore = roomWithout(before, fragments, name, id);
        await enter(page, r);
        await page.keyboard.type(`${word} `);
        await expect
          .poll(async () => (await getArtifactText(artifacts[artifact].id)).markdown)
          .toContain(word);
        const after = await snapshot(browser, seed.key);
        try {
          expect(after.getXmlFragment(name).toString()).toContain(word);
          expect(roomWithout(after, fragments, name, id)).toEqual(snapshotBefore);
        } finally {
          after.destroy();
        }
        await leaveEditor(page, r);
      } finally {
        before.destroy();
      }
    }
    expect(sockets()).toBe(1);
  });
});

// SLOW(>=2s): the real seed, a live selection, and the outline's comment composer.
test("the block editor keeps the comment-on-selection chord @phone", async ({ browser }) => {
  test.setTimeout(ROW_TIMEOUT);
  await inSeed(browser, async ({ key }, page) => {
    await openOutline(page, key, FIRST);
    await ready(page);
    await enter(page, firstRow(page));
    const quote = (await rowEditor(firstRow(page)).innerText()).trim().split(/\s+/, 4).join(" ");
    expect(quote).not.toBe("");
    await selectEditorText(page, quote);
    await page.keyboard.press("ControlOrMeta+Alt+KeyM");
    const form = composer(page);
    await expect(form).toContainText(quote);
    await expect(form.getByLabel("Comment", { exact: true })).toBeFocused();
  });
});

// SLOW(>=2s): the seed and both a placed click and Enter on a prepared live editor.
test("a click on a row's text opens the block editor with the caret at the pressed place @phone", async ({
  browser,
}) => {
  test.setTimeout(ROW_TIMEOUT);
  await inSeed(browser, async ({ key }, page) => {
    await openOutline(page, key, FIRST);
    await ready(page);
    const row = firstRow(page);
    await clickAt(page, row.locator(WORDS), 5);
    await expect(rowEditor(row)).toBeFocused();
    await expect.poll(() => caretIn(rowEditor(row))).toBe(5);
    await leaveEditor(page, row);
    await enter(page, row);
    await expect.poll(() => caretIn(rowEditor(row))).toBe(0);
  });
});

// SLOW(>=2s): the seed, a second browser, and controlled delivery of two live removal cases.
test("the block editor ends and reports when another writer deletes or merges the held block @phone", async ({
  browser,
}) => {
  test.setTimeout(ROW_TIMEOUT);
  const seed = await seedLargestShape();
  for (const removal of ["delete", "merge"] as const) {
    const removed = removal === "delete" ? FIRST : SECOND;
    await inReader(browser, "alice", true, async (page) => {
      let hold = false;
      const release: (() => void)[] = [];
      await page.routeWebSocket(/\/ws\/doc\//, (socket) => {
        const server = socket.connectToServer();
        socket.onMessage((message) => {
          if (hold && syncPayload(message, messageYjsUpdate)) {
            release.push(() => server.send(message));
          } else server.send(message);
        });
      });
      await openOutline(page, seed.key, removed);
      await ready(page);
      const row = rowOf(page, removed);
      await enter(page, row);
      hold = true;
      await page.keyboard.type("notkept ");
      await expect
        .poll(() => release.length, { message: "the local edit is in flight" })
        .toBeGreaterThan(0);
      await inReader(browser, "bob", false, async (bob) => {
        await openWholeSpec(bob, seed.key);
        const deleteBlock = { block: removed, op: "delete" as const };
        if (removal === "merge") {
          const surviving = documentEditor(bob).locator('[data-block-id="r3"] p').first();
          const target = await surviving.innerText();
          const source = await documentEditor(bob)
            .locator(`[data-block-id="${removed}"] p`)
            .first()
            .innerText();
          await editArtifact(
            seed.specArtifactId,
            {
              ops: [{ find: target, with: `${target} ${source}`, op: "replace" }, deleteBlock],
            },
            { login: "bob" }
          );
          await expect(surviving).toContainText(source);
        } else {
          await editArtifact(seed.specArtifactId, { ops: [deleteBlock] }, { login: "bob" });
        }
        await expect(documentEditor(bob).locator(`[data-block-id="${removed}"]`)).toHaveCount(0);
      });
      // Keep the outgoing edit in flight until the other browser's removal reached this editor.
      await expect(row.locator(EDITOR_BOX_SELECTOR)).toHaveCount(0);
      if (removal === "delete") {
        await expect(page.getByRole("status").filter({ hasText: /not saved/i })).toBeVisible();
      } else {
        // The server writes the merged block into the held block's own element, by its place
        // (`pmdoc/update.go`'s `updateChildren`), so the room keeps the edit in flight there:
        // the status says the block was removed, and not that the typing was lost.
        const status = page.getByRole("status").filter({ hasText: /was removed/i });
        await expect(status).toBeVisible();
        await expect(status).not.toContainText(/not saved/i);
      }
      hold = false;
      for (const send of release.splice(0)) send();
      if (removal === "merge") {
        await expect
          .poll(async () => (await getArtifactText(seed.specArtifactId)).markdown, {
            message: "the room does not keep the typing in flight as the merge ended the editor",
          })
          .toContain("notkept");
      }
      const before = (await getArtifactText(seed.specArtifactId)).markdown;
      await page.keyboard.type("afterremoval");
      expect((await getArtifactText(seed.specArtifactId)).markdown).toBe(before);
    });
  }
});

// SLOW(>=2s): the server installs a real schema migration, reconnects the editor, and a peer reads its edit.
test("an open block editor stays writable when the block schema changes @phone", async ({
  browser,
}) => {
  test.setTimeout(ROW_TIMEOUT);
  await inSeed(browser, async (seed, page) => {
    const frames = roomFrames(page);
    await openOutline(page, seed.key, FIRST);
    await ready(page);
    await enter(page, firstRow(page));
    expect(await servesAddedType()).toBe(false);
    try {
      await applySupertagMigration();
      await expect.poll(() => servesAddedType()).toBe(true);
      await frames.frame(1);
      await expect(rowEditor(firstRow(page))).toBeFocused();
      await page.keyboard.type("aftertheswitch ");
      await inReader(browser, "bob", false, async (bob) => {
        await openWholeSpec(bob, seed.key);
        await expect(documentEditor(bob).locator(`[data-block-id="${FIRST}"]`)).toContainText(
          "aftertheswitch"
        );
      });
      await expect(page.getByText(/reload to edit/i)).toHaveCount(0);
    } finally {
      await restoreStoreSchema();
      await expect.poll(() => servesAddedType()).toBe(false);
    }
  });
});

// SLOW(>=2s): seed the real hard break and compare the editor's arrow with the outline's arrow.
test("ArrowDown keeps the caret in the block after a hard break @phone", async ({ browser }) => {
  test.setTimeout(ROW_TIMEOUT);
  await inSeed(browser, async (seed, page) => {
    // Under the post r1 answers, beside r1, so the outline draws rows below it as below r1: a
    // statement that answers nothing stands under the issue, where it was the outline's last row.
    const parent = seed.origins[FIRST];
    if (parent === undefined) throw new Error(`the seed's ${FIRST} answers no post`);
    await editArtifact(seed.specArtifactId, {
      ops: [
        {
          after: `block:${FIRST}`,
          markdown: `:::requirement{#r-hard-break parent="${parent}"}\nFirst line.  \nLast line.\n:::\n`,
          op: "insert",
        },
      ],
    });
    await openOutline(page, seed.key, "r-hard-break");
    await ready(page);
    const row = rowOf(page, "r-hard-break");
    await enter(page, row);
    const editor = rowEditor(row);
    // A `<br>` has no width, which Playwright reads as hidden whatever draws it.
    await expect(editor.locator("br")).toHaveCount(1);
    await editor.evaluate((element) => {
      const afterBreak = element.querySelector("br")?.nextSibling;
      if (!afterBreak) throw new Error("no text after the hard break");
      document.getSelection()?.collapse(afterBreak, 0);
      document.dispatchEvent(new Event("selectionchange"));
    });
    expect(await caretIn(editor)).toBe("First line.".length);
    await page.keyboard.press("ArrowDown");
    await expect(editor).toBeFocused();
    expect(await caretIn(editor)).not.toBeNull();
    await leaveEditor(page, row);
    const next = await row.evaluate((element) => {
      const rows = [...document.querySelectorAll("[data-issue-body] [data-node-row]")].filter(
        (row) => row.getClientRects().length > 0
      );
      return rows[rows.indexOf(element) + 1]?.getAttribute("data-row-key");
    });
    expect(next, "a visible outline row stands below the block").toBeTruthy();
    await page.keyboard.press("ArrowDown");
    await expect(page.locator("[data-node-row]:focus")).toHaveAttribute("data-row-key", next ?? "");
  });
});

// SLOW(>=2s): two fresh readers each type while a real first sync is held for 2 s.
test("an Enter before the block editor is ready draws the caret at once and keeps every key @phone", async ({
  browser,
}) => {
  test.setTimeout(ROW_TIMEOUT);
  const seed = await seedLargestShape();
  for (const useClick of [false, true]) {
    const id = useClick ? SECOND : FIRST;
    await inReader(browser, "alice", true, async (page) => {
      const held = Promise.withResolvers<void>();
      const typed = Promise.withResolvers<void>();
      await page.routeWebSocket(/\/ws\/doc\//, (socket) => {
        const server = socket.connectToServer();
        let pause: Promise<void> | undefined;
        server.onMessage((message) => {
          if (pause === undefined && syncPayload(message, messageYjsSyncStep2)) {
            held.resolve();
            // The hold is the scenario: input must work before the first real sync reaches the tab.
            pause = Promise.all([delay(2_000), typed.promise]).then(() => {});
          }
          if (pause === undefined) socket.send(message);
          else void pause.then(() => socket.send(message));
        });
      });
      await openOutline(page, seed.key, id);
      const row = rowOf(page, id);
      const words = row.locator(WORDS);
      const before = (await words.innerText()).trim();
      const offset = useClick ? 5 : 0;
      if (useClick) await clickAt(page, words, offset);
      else {
        await row.focus();
        await page.keyboard.press("Enter");
      }
      const standIn = row.locator(`${EDITOR_BOX_SELECTOR} [contenteditable="plaintext-only"]`);
      try {
        await expect(standIn).toBeFocused();
        expect(await caretIn(standIn)).toBe(offset);
        expect(await readMark(page, BLOCK_EDITOR_READY_MARK)).toBeNull();
        await held.promise;
        if (useClick) await page.keyboard.type("x");
        else {
          const cdp = await page.context().newCDPSession(page);
          await page.keyboard.type("abc");
          await cdp.send("Input.imeSetComposition", {
            text: "に",
            selectionStart: 1,
            selectionEnd: 1,
          });
          await cdp.send("Input.imeSetComposition", {
            text: "にほ",
            selectionStart: 2,
            selectionEnd: 2,
          });
          await cdp.send("Input.insertText", { text: "日本" });
          await page.keyboard.type("de");
        }
      } finally {
        typed.resolve();
      }
      const inserted = useClick ? "x" : "abc日本de";
      const expected = before.slice(0, offset) + inserted + before.slice(offset);
      await ready(page);
      await expect(standIn).toHaveCount(0);
      await expect(rowEditor(row)).toContainText(expected);
      await expect.poll(() => caretIn(rowEditor(row))).toBe(offset + inserted.length);
      await inReader(browser, "bob", false, async (bob) => {
        await openWholeSpec(bob, seed.key);
        // Bob's editor draws alice's caret, which stands after `e`, with her name, in the block's
        // text; the room's text is read without it.
        const block = documentEditor(bob).locator(`[data-block-id="${id}"]`);
        await expect
          .poll(() =>
            block.evaluate((element) => {
              const text = element.cloneNode(true) as Element;
              for (const cursor of text.querySelectorAll(".ProseMirror-yjs-cursor")) {
                cursor.remove();
              }
              return text.textContent ?? "";
            })
          )
          .toContain(expected);
      });
    });
  }
});
