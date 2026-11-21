import type { ArtifactText } from "@legion/contracts";
import { type Browser, expect, type Page, test } from "@playwright/test";
import { createDecoder, readVarUint } from "lib0/decoding";
import * as Y from "yjs";

import { retiredClients } from "../web/src/features/doc/pending-sync";
import { readField } from "../web/src/features/doc/sync-frame";
import {
  createIssue,
  createIssueArtifact,
  createProject,
  dashboardOrigin,
  editArtifact,
  getArtifactText,
  patchIssue,
  quiesceDocuments,
} from "./api";
import {
  connectedDot,
  countDocumentSockets,
  documentEditor,
  documentTransport,
  openSpec,
  paste,
  pendingEditCount,
  placeCaret,
  typeAtEnd,
} from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// What a person sees when the document server refuses an edit: as too large for the document
// (DOCUMENT_TOO_LARGE_CLOSE_CODE, permanent: the edit is dropped), as one it could not merge into
// the document quickly enough (the same code with DOC_TOO_TANGLED, permanent too: the edit is
// dropped), as one the document cannot take until it is rebuilt (the same code with
// DOC_NEEDS_REBUILD: the edit is dropped and the rebuild is offered), as one typed against the
// history someone else's rebuild replaced (the same code with DOC_REBUILT: the edit is dropped,
// the document is read again and no rebuild is offered), as one holding content the
// document does not take (the same code with DOC_UPDATE_REFUSED, permanent: the edit is dropped),
// or for want of room for open documents (DOCUMENT_BUSY_CLOSE_CODE, transient: the edit waits and
// saves once room frees). A document whose own read finds it near the decode cap is offered the
// rebuild before anyone is refused, and one far from the cap is offered none.

const TOO_LARGE = "Your last edit was not saved: it would make this document larger";
const NEEDS_REBUILD =
  "This document has been edited so much that it must be rebuilt before it can take this edit. Your other edits are kept.";
const REBUILT_ELSEWHERE =
  "Someone else rebuilt this document, so the edits you had not sent could not be kept. Dispatch has reloaded the rebuilt document.";
const TOO_TANGLED =
  "Your last edit was not saved: Dispatch could not merge it into this document quickly enough. Your other edits are kept.";
const UPDATE_REFUSED =
  "Your last edit was not saved: it holds content this document does not take.";
const OUT_OF_ROOM = "Dispatch is out of room for open documents right now.";
const NEAR_CAP =
  "This document has been edited so much that it must be rebuilt before it can take larger changes.";
const ROOM_BUDGET = "/api/v1/artifacts/_test/room-budget";
/** The sentence both browsers of the used-up row write after, the end of its paragraph's text. */
const MEETING_POINT = "Both browsers write after this sentence.";
/** What one browser types before its caret, each keystroke at the same place: digits in turn, so
 * no two neighbours are equal and the editor's diff inserts each where it was typed. A hundred of
 * them are a hundred items between the place and the end of its text, more than the 64 a used-up
 * room lets one browser's apply scan (usedUpScanBudget). */
const TYPED_IN_PLACE = Array.from({ length: 100 }, (_, index) => String(index % 10)).join("");
/** The paragraph the tangle row types, the end of the document's last text: its last character is
 * the left origin of both the paste that browser makes offline and the tangle chained on after
 * it, which is what makes merging the paste scan the whole tangle. */
const TANGLE_POINT = "This paragraph is where the tangle starts.";

/** The most markdown one document may hold (pmdoc.MaxDocumentBytes). */
const MAX_DOCUMENT_BYTES = 1 << 20;
/** More than the 8 KiB the server admits into a document at its bound (maxPeerInsertAtTheBound). */
const REFUSED_PASTE = `refused-paste ${"x".repeat(10_000)}`;
/** A row that opens a document at the bound loads and syncs a mebibyte of it, more than once, so
 * it runs past the suite's 30-second default: about 17 and 24 seconds here in Chromium. */
const AT_THE_BOUND_TIMEOUT_MS = 90_000;

/** Markdown about 2 KiB under the document bound, in plain paragraphs: a paste of `REFUSED_PASTE`
 * takes it past the bound, and a keystroke does not. */
function markdownAtTheBound(): string {
  const paragraph = `${"Prose at the document bound.".repeat(36)}\n\n`;
  let markdown = "# At the bound\n\n";
  while (markdown.length + paragraph.length <= MAX_DOCUMENT_BYTES - 2048) {
    markdown += paragraph;
  }
  return markdown;
}

/** The marker only the last frames of a many-frame sync carry: it is the document's last
 * paragraph, so a client that stopped at any earlier frame does not hold it. */
const LAST_PARAGRAPH_MARKER = "the last paragraph of a many-frame document";

/** The same markdown with that marker as its last paragraph. */
function markdownWithALastParagraph(): string {
  return `${markdownAtTheBound()}${LAST_PARAGRAPH_MARKER}\n`;
}

async function documentAtTheBound(title: string) {
  await createProject({ key: "BOUND", name: "Document bounds" });
  const issue = await createIssue({ project: "BOUND", title });
  const { artifact } = await createIssueArtifact(issue.key, {
    content: markdownAtTheBound(),
    name: "at-the-bound.md",
  });
  return { artifact, issue, path: `/issues/${issue.key}/artifacts/${artifact.slug}` };
}

/** What a page's document sockets were told, as a row reads it: the close code of every document
 *  socket the page saw close, in order; how many document sockets it opened; and the flag of every
 *  SyncStatus frame one received, 1 applied and 0 not. It carries no frame: the frames are read
 *  once, by `replacedHistoryHandoffs`, and only by the rows that count handovers. */
interface DocumentSockets {
  closes: number[];
  opened: number;
  statuses: number[];
}

/** In the page: every SyncStep2 and Update frame one document socket sent, and every one the
 *  server sent it, each the whole frame as it crossed, in order. They stay bytes in the page. A
 *  record that copied each frame into an array of numbers and handed every frame back on every
 *  read cost the rows that read only closes and SyncStatus answers most of their time: a document
 *  at the bound is forty-odd frames of 64 KiB, and the read-only row polls the record. */
interface RecordedSocket {
  received: Uint8Array[];
  sent: Uint8Array[];
}

interface RecordedSockets {
  closes: number[];
  sockets: RecordedSocket[];
  statuses: number[];
}

declare global {
  interface Window {
    documentSockets?: RecordedSockets;
  }
}

/** Records, inside every page `page` loads, what its document sockets were told and handed: the
 * close codes the server closed them with, the SyncStatus answers it gave, and the update frames
 * each socket carried either way. Install before navigating. Answers a read of all but the frames
 * (`DocumentSockets`); `replacedHistoryHandoffs` reads those. */
async function recordDocumentSockets(page: Page): Promise<() => Promise<DocumentSockets>> {
  await page.addInitScript(() => {
    const record: RecordedSockets = { closes: [], sockets: [], statuses: [] };
    window.documentSockets = record;
    const readVarUint = (bytes: Uint8Array, at: { pos: number }): number => {
      let value = 0;
      for (let shift = 0; ; shift += 7) {
        const byte = bytes[at.pos] ?? 0;
        at.pos += 1;
        value += (byte & 0x7f) * 2 ** shift;
        if (byte < 0x80) {
          return value;
        }
      }
    };
    // A frame is the document's name, then its tag: 8 is Hocuspocus's SyncStatus, the server's
    // answer to each update frame it was sent, and 0 a Sync frame, whose step 1 (SyncStep2) and 2
    // (Update) carry a Yjs update. Answers the SyncStatus flag, whether the frame carries an
    // update, or neither.
    const read = (bytes: Uint8Array): { status?: number; update: boolean } => {
      const at = { pos: 0 };
      const nameLength = readVarUint(bytes, at);
      at.pos += nameLength; // the document name
      const tag = readVarUint(bytes, at);
      if (tag === 8) {
        return { status: readVarUint(bytes, at), update: false };
      }
      if (tag !== 0) {
        return { update: false };
      }
      const step = readVarUint(bytes, at);
      return { update: step === 1 || step === 2 };
    };
    const Native = window.WebSocket;
    window.WebSocket = class extends Native {
      recorded: RecordedSocket | undefined;

      constructor(url: string | URL, protocols?: string | string[]) {
        super(url, protocols);
        if (!new URL(String(url), window.location.href).pathname.startsWith("/ws/doc/")) {
          return;
        }
        const recorded: RecordedSocket = { received: [], sent: [] };
        this.recorded = recorded;
        record.sockets.push(recorded);
        this.addEventListener("close", (event) => record.closes.push(event.code));
        this.addEventListener("message", (event) => {
          if (!(event.data instanceof ArrayBuffer)) {
            return;
          }
          const bytes = new Uint8Array(event.data);
          const { status, update } = read(bytes);
          if (status !== undefined) {
            record.statuses.push(status);
          } else if (update) {
            // The event's buffer is this frame's own, so the frame is kept as it came, uncopied.
            recorded.received.push(bytes);
          }
        });
      }

      override send(data: string | ArrayBufferLike | Blob | ArrayBufferView): void {
        if (this.recorded !== undefined && data instanceof Uint8Array && read(data).update) {
          this.recorded.sent.push(data.slice());
        }
        super.send(data);
      }
    };
  });
  return async () => {
    // The init script sets this record before the page's own code runs, so a page loaded after
    // it was installed holds it from its first instant and one loaded before never will.
    const record = await page.evaluate(() => {
      const recorded = window.documentSockets;
      return recorded === undefined
        ? undefined
        : { closes: recorded.closes, opened: recorded.sockets.length, statuses: recorded.statuses };
    });
    if (record === undefined) {
      throw new Error("the page loaded before recordDocumentSockets installed its script");
    }
    return record;
  };
}

/** The Yjs update a SyncStep2 or Update frame carries. */
function updateOf(frame: Uint8Array): Uint8Array {
  const decoder = createDecoder(frame);
  readField(decoder); // the document name
  readVarUint(decoder); // the Sync tag
  readVarUint(decoder); // the step
  return readField(decoder);
}

/**
 * The sockets `page` opened after its first `from`, on which it handed the document server the
 * history a rebuild replaced, each as the size of the update that carried it: a struct of a client
 * the rebuilt document retired, below the clock it lists, which the server refuses as DOC_REBUILT
 * (envoy's `weighPeerUpdate`, whose log line names the same `update_bytes`). The list of retired
 * clients is the one the server sent the page on any of its sockets. This reads what the page sent
 * rather than the closes it saw: the server closes the socket at once after its refusal, and a
 * close frame the connection's reset overtakes reaches the page as an abnormal closure (1006),
 * with no reason. The server closes a socket at its first refusal, so each counts once. The
 * frames cross from the page once, here, as base64, and are decoded on this side, where Yjs is.
 */
async function replacedHistoryHandoffs(page: Page, from: number): Promise<number[]> {
  // One instant's record is what this reads: its callers have waited for what these frames
  // caused, and the init script's record is there from the page's first instant or never.
  const frames = await page.evaluate((first) => {
    const record = window.documentSockets;
    if (record === undefined) {
      return undefined;
    }
    const base64 = (bytes: Uint8Array): string => {
      let binary = "";
      for (let at = 0; at < bytes.length; at += 0x8000) {
        binary += String.fromCharCode(...bytes.subarray(at, at + 0x8000));
      }
      return btoa(binary);
    };
    return {
      received: record.sockets.flatMap((socket) => socket.received.map(base64)),
      sent: record.sockets.slice(first).map((socket) => socket.sent.map(base64)),
    };
  }, from);
  if (frames === undefined) {
    throw new Error("the page loaded before recordDocumentSockets installed its script");
  }
  const updateIn = (frame: string) => updateOf(new Uint8Array(Buffer.from(frame, "base64")));
  const told = new Y.Doc();
  for (const frame of frames.received) {
    Y.applyUpdate(told, updateIn(frame));
  }
  const retired = retiredClients(told);
  const handoffs: number[] = [];
  for (const sent of frames.sent) {
    const replaced = sent
      .map(updateIn)
      .find((update) =>
        Y.decodeUpdate(update).structs.some(({ id }) => id.clock < (retired.get(id.client) ?? 0))
      );
    if (replaced !== undefined) {
      handoffs.push(replaced.length);
    }
  }
  return handoffs;
}

// `page.request` sends the page's session cookie and no `Origin`, and the server refuses a
// cookie-authenticated write whose `Origin` is not the dashboard's (enforceCookieOrigin).
function postAsPage(page: Page, path: string, data?: object) {
  return page.request.post(path, { data, headers: { Origin: dashboardOrigin } });
}

/** The document's text read as the dashboard makes it, asking for display trees
 *  (`Dispatch-Trees: 1`): the read that says whether the rebuild route takes the document by the
 *  decode cap (`near_decode_cap`) and, where it does, why. */
async function dashboardTextRead(page: Page, artifactId: string): Promise<ArtifactText> {
  const response = await page.request.get(`/api/v1/artifacts/${artifactId}/text`, {
    headers: { "Dispatch-Trees": "1" },
  });
  expect(response.status(), await response.text()).toBe(200);
  return (await response.json()) as ArtifactText;
}

test.beforeEach(async () => {
  await resetDatabase();
});

// A document whose state needs many sync frames loads whole in the browser and takes an edit.
// The server answers a browser's SyncStep1 with the room's state as Update frames of about
// 64 KiB each and then one SyncStep2 carrying only what no Update could (sjawhar/ygo
// v1.55.1-sami.3), so a client that applied the first frame alone would show an empty document
// and one that stopped at any frame a truncated one. A document at the bound is about 3 MB of
// state, forty-odd frames, which is the case this drives: the heading the first frame carries,
// the marker only the last frames do, the whole markdown's length, and an edit after them that
// saves. Nothing here is the dashboard's own code: it is y-protocols applying what the server
// sends, which is why no server-side test can stand for it.
test("a document whose state needs many sync frames loads whole and takes an edit", async ({
  browser,
}) => {
  test.setTimeout(AT_THE_BOUND_TIMEOUT_MS);
  await createProject({ key: "BOUND", name: "Document bounds" });
  const issue = await createIssue({ project: "BOUND", title: "Load a many-frame document" });
  const markdown = markdownWithALastParagraph();
  const { artifact } = await createIssueArtifact(issue.key, {
    content: markdown,
    name: "many-frames.md",
  });
  const path = `/issues/${issue.key}/artifacts/${artifact.slug}`;
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const sockets = await recordDocumentSockets(page);
    await page.goto(path);
    await expect(documentEditor(page)).toContainText("At the bound");
    await expect(connectedDot(page)).toHaveText("connected");
    // The marker is the document's last paragraph, so a sync that stopped early does not hold
    // it, and the length holds every frame between the two.
    await expect(documentEditor(page)).toContainText(LAST_PARAGRAPH_MARKER);
    const shown = (await documentEditor(page).innerText()).length;
    expect(shown).toBeGreaterThan(markdown.length * 0.9);

    await typeAtEnd(page, "saved after a many-frame sync");
    await expect
      .poll(async () => (await getArtifactText(artifact.id)).markdown)
      .toContain("saved after a many-frame sync");
    await expect.poll(() => pendingEditCount(page, artifact.id)).toBe(0);
    // The whole state arrived over one socket, with no close and no reconnect.
    expect((await sockets()).closes).toHaveLength(0);
    expect((await getArtifactText(artifact.id)).markdown).toContain(LAST_PARAGRAPH_MARKER);
  } finally {
    await alice.close();
  }
});

/** The connection indicator while it names edits this browser holds that have not reached the
 *  server: the notice is the sentence after the connection's own name (`ConnectionDot`). */
function unsentNotice(page: Page) {
  return page.getByRole("status", {
    name: /saved in this browser, not sent to Dispatch yet\. Dispatch keeps trying\./,
  });
}

test("a stalled handover names the edits this browser is still holding", async ({ browser }) => {
  await createProject({ key: "UNSENT", name: "Unsent edits" });
  const issue = await createIssue({
    project: "UNSENT",
    spec: "# Unsent\n\nOne paragraph of ordinary prose.\n",
    title: "Name the edits a stalled server has not taken",
  });
  const artifactId = issue.primary_artifact_id;
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const transport = await documentTransport(page);
    await openSpec(page, issue.key, "One paragraph of ordinary prose.");

    // Only the server's acknowledgements are held: the socket stays open and synced and the
    // page stays connected and writable, which is the state a stalled handover leaves it in and
    // the one neither the offline nor the read-only sentence covers.
    transport.holdServerFrames();
    await typeAtEnd(page, "typed while nothing is acknowledged");
    await expect.poll(() => pendingEditCount(page, artifactId)).toBeGreaterThan(0);
    await expect(unsentNotice(page)).toBeVisible();

    // The acknowledgements arrive, the rows land, and the sentence goes with them.
    await transport.releaseServerFrames();
    await expect.poll(() => pendingEditCount(page, artifactId)).toBe(0);
    await expect(unsentNotice(page)).toHaveCount(0);
    await expect(connectedDot(page)).toHaveText("connected");
    expect((await getArtifactText(artifactId)).markdown).toContain(
      "typed while nothing is acknowledged"
    );
  } finally {
    await alice.close();
  }
});

/**
 * A pending offline edit replays on the next page, which is what the refusal rows below rest on:
 * each of them pastes offline, closes that page and expects the saved edit to reach the server
 * from the page after it. These two rows prove the replay itself, once into a document of one
 * paragraph and once into one whose state needs many sync frames, since this change hands a
 * document's state over as a series of bounded frames (sjawhar/ygo v1.55.1-sami.3 on) where one
 * frame carried it before, and an editor that replays only after a sync it sees completed would
 * lose the edit in the second where it keeps it in the first (INFRA-179).
 */
async function replaysAPendingPaste(
  browser: Browser,
  issueKey: string,
  artifactId: string,
  path: string,
  anchor: string
): Promise<void> {
  const alice = await asUser(browser, "alice");
  try {
    const offline = await alice.newPage();
    const transport = await documentTransport(offline);
    await offline.goto(path);
    await expect(documentEditor(offline)).toContainText(anchor);
    await expect(connectedDot(offline)).toHaveText("connected");

    transport.hold();
    await transport.sever();
    await paste(offline, { html: "", text: "pasted while offline" });
    await expect.poll(() => pendingEditCount(offline, artifactId)).toBe(1);
    await offline.close();

    const page = await alice.newPage();
    await page.goto(path);
    await expect(connectedDot(page)).toHaveText("connected");
    await expect
      .poll(async () => (await getArtifactText(artifactId)).markdown, { timeout: 60_000 })
      .toContain("pasted while offline");
    await expect.poll(() => pendingEditCount(page, artifactId)).toBe(0);
  } finally {
    await alice.close();
  }
  void issueKey;
}

test("a pending offline paste replays into a document of one paragraph", async ({ browser }) => {
  test.setTimeout(AT_THE_BOUND_TIMEOUT_MS);
  await createProject({ key: "REPLAY", name: "Pending replays" });
  const issue = await createIssue({
    project: "REPLAY",
    spec: "# Replay\n\nOne paragraph of ordinary prose.\n",
    title: "Replay a pending paste into a small document",
  });
  await replaysAPendingPaste(
    browser,
    issue.key,
    issue.primary_artifact_id,
    `/issues/${issue.key}/spec`,
    "One paragraph of ordinary prose."
  );
});

test("a pending offline paste replays into a document whose state needs many sync frames", async ({
  browser,
}) => {
  test.setTimeout(AT_THE_BOUND_TIMEOUT_MS);
  await createProject({ key: "REPLAYBIG", name: "Pending replays over many frames" });
  const issue = await createIssue({
    project: "REPLAYBIG",
    title: "Replay a pending paste into a many-frame document",
  });
  const { artifact } = await createIssueArtifact(issue.key, {
    content: markdownWithALastParagraph(),
    name: "many-frames.md",
  });
  await replaysAPendingPaste(
    browser,
    issue.key,
    artifact.id,
    `/issues/${issue.key}/artifacts/${artifact.slug}`,
    "At the bound"
  );
});

test("a pending offline paste replays into the tangle row's own document when the hook never runs", async ({
  browser,
}) => {
  test.setTimeout(AT_THE_BOUND_TIMEOUT_MS);
  await createProject({ key: "RETANGLE", name: "Pending replays without the tangle" });
  const issue = await createIssue({
    project: "RETANGLE",
    spec: "# Tangled\n\nThe tangle follows the next paragraph.\n",
    title: "Replay a pending paste with no chain laid",
  });
  await replaysAPendingPaste(
    browser,
    issue.key,
    issue.primary_artifact_id,
    `/issues/${issue.key}/spec`,
    "The tangle follows the next paragraph."
  );
});

test("an edit past the document bound is dropped with its reason, and the next edit saves", async ({
  browser,
}) => {
  test.setTimeout(AT_THE_BOUND_TIMEOUT_MS);
  const { artifact, path } = await documentAtTheBound("Refuse a paste past the bound");
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const sockets = await recordDocumentSockets(page);
    await page.goto(path);
    await expect(documentEditor(page)).toContainText("Prose at the document bound.");
    await expect(connectedDot(page)).toHaveText("connected");

    await typeAtEnd(page, "kept before the paste");
    await expect
      .poll(async () => (await getArtifactText(artifact.id)).markdown)
      .toContain("kept before the paste");
    await paste(page, { html: "", text: REFUSED_PASTE });

    await expect(page.getByText(TOO_LARGE)).toBeVisible();
    await expect(connectedDot(page)).toHaveText("connected");
    await expect(documentEditor(page)).toContainText("kept before the paste");
    await expect(documentEditor(page)).not.toContainText("refused-paste");
    await expect.poll(() => pendingEditCount(page, artifact.id)).toBe(0);
    expect((await getArtifactText(artifact.id)).markdown).not.toContain("refused-paste");

    await typeAtEnd(page, "saved after the refusal");
    await expect
      .poll(async () => (await getArtifactText(artifact.id)).markdown)
      .toContain("saved after the refusal");
    await expect.poll(() => pendingEditCount(page, artifact.id)).toBe(0);
    // One refusal, and no reconnect that met it again.
    expect((await sockets()).closes.filter((code) => code === 4413)).toHaveLength(1);
    expect((await getArtifactText(artifact.id)).markdown).not.toContain("refused-paste");
  } finally {
    await alice.close();
  }
});

test("a read-only viewer's saved edits are held through a not-applied answer, then sent", async ({
  browser,
}) => {
  test.setTimeout(AT_THE_BOUND_TIMEOUT_MS);
  const { artifact, issue, path } = await documentAtTheBound("Hold edits while read-only");
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const sockets = await recordDocumentSockets(page);
    const transport = await documentTransport(page);
    await page.goto(path);
    await expect(documentEditor(page)).toContainText("Prose at the document bound.");
    await expect(connectedDot(page)).toHaveText("connected");

    transport.hold();
    await transport.sever();
    await typeAtEnd(page, "held while read-only");
    await paste(page, { html: "", text: REFUSED_PASTE });
    await expect
      .poll(() => pendingEditCount(page, artifact.id))
      .toBeGreaterThan("held while read-only".length);
    const held = await pendingEditCount(page, artifact.id);

    // The reconnect is admitted read-only, and the server answers the edits it carries as not
    // applied: the browser keeps every one of them, the paste included, and is not closed. A
    // closed issue's read-only admission names no reload - a reload cannot make it editable - so
    // the editor itself is what says the admission landed (close-withdraws-asks.e2e.ts:158-167).
    await patchIssue(issue.key, { status: "done" });
    await transport.release();
    await expect(documentEditor(page)).toHaveAttribute("contenteditable", "false");
    await expect.poll(async () => (await sockets()).statuses).toContain(0);
    expect(await pendingEditCount(page, artifact.id)).toBe(held);
    expect((await sockets()).closes).not.toContain(4413);
    const readOnlyText = (await getArtifactText(artifact.id)).markdown;
    expect(readOnlyText).not.toContain("held while read-only");
    expect(readOnlyText).not.toContain("refused-paste");

    // Read-write again, the edits are sent: the paste past the bound is dropped with its reason,
    // and the rest saves.
    await patchIssue(issue.key, { status: "backlog" });
    await page.reload();
    await expect(page.getByText(TOO_LARGE)).toBeVisible();
    await expect
      .poll(async () => (await getArtifactText(artifact.id)).markdown)
      .toContain("held while read-only");
    await expect.poll(() => pendingEditCount(page, artifact.id)).toBe(0);
    await expect(connectedDot(page)).toHaveText("connected");
    expect((await getArtifactText(artifact.id)).markdown).not.toContain("refused-paste");
    expect((await sockets()).closes.filter((code) => code === 4413)).toHaveLength(1);
  } finally {
    await alice.close();
  }
});

test("a full room budget turns a document away and holds an editor's typing until room frees", async ({
  browser,
}) => {
  // Each refused attempt waits longer than the last (the provider's backoff, up to 30 seconds),
  // so the attempt that finds room can come that long after room frees.
  test.setTimeout(150_000);
  await createProject({ key: "ROOM", name: "Room budget" });
  const editing = await createIssue({
    project: "ROOM",
    spec: "# Open first\n\nThe document already open.\n",
    title: "Already editing",
  });
  const waiting = await createIssue({
    project: "ROOM",
    spec: "# Opened second\n\nThe document asked for after.\n",
    title: "Turned away",
  });
  // No room is resident but the one the editor below opens: a resident room is joined at no cost
  // whatever the budget.
  await quiesceDocuments();
  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  try {
    await openSpec(page, editing.key, "The document already open.");
    // A budget of one byte: the open room is in use, so nothing can be closed.
    const full = await postAsPage(page, ROOM_BUDGET, { bytes: 1 });
    expect(full.status(), await full.text()).toBe(200);

    const turnedAway = await alice.newPage();
    const attempts = countDocumentSockets(turnedAway, waiting.primary_artifact_id);
    await turnedAway.goto(`/issues/${waiting.key}/spec`);
    await expect(turnedAway.getByText(OUT_OF_ROOM)).toBeVisible();
    await expect.poll(attempts).toBeGreaterThanOrEqual(2);
    await expect(turnedAway.getByText(/reload the page/u)).toHaveCount(0);

    await typeAtEnd(page, "typed while out of room");
    await expect(page.getByText(OUT_OF_ROOM)).toBeVisible();
    await expect(documentEditor(page)).toContainText("typed while out of room");
    await expect.poll(() => pendingEditCount(page, editing.primary_artifact_id)).toBeGreaterThan(0);
    expect((await getArtifactText(editing.primary_artifact_id)).markdown).not.toContain(
      "typed while out of room"
    );

    const freed = await postAsPage(page, ROOM_BUDGET, { bytes: 0 });
    expect(freed.status(), await freed.text()).toBe(200);
    await expect
      .poll(async () => (await getArtifactText(editing.primary_artifact_id)).markdown, {
        timeout: 60_000,
      })
      .toContain("typed while out of room");
    await expect(page.getByText(OUT_OF_ROOM)).toHaveCount(0);
    await expect.poll(() => pendingEditCount(page, editing.primary_artifact_id)).toBe(0);
    await expect(connectedDot(turnedAway)).toHaveText("connected", { timeout: 60_000 });
    await expect(documentEditor(turnedAway)).toContainText("The document asked for after.");
    await expect(turnedAway.getByText(OUT_OF_ROOM)).toHaveCount(0);
  } finally {
    // The budget is the server's, and every later row shares it.
    await postAsPage(page, ROOM_BUDGET, { bytes: 0 });
    await alice.close();
  }
});

test("a used-up document refuses a larger merge as needing a rebuild, and saves after the rebuild it offers", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await createProject({ key: "USED", name: "Used-up documents" });
  const issue = await createIssue({
    project: "USED",
    spec: `${MEETING_POINT}\n\nThe tangle follows.\n`,
    title: "Rebuild a used-up document",
  });
  const artifactId = issue.primary_artifact_id;
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");
  try {
    const offline = await alice.newPage();
    const transport = await documentTransport(offline);
    // The hook appends a tangle to the stored history whose re-scan uses up a room's scan total
    // (docs.Service.UseUpScanTotalForTest), at the end of the last paragraph: the room every
    // browser opens below is used up, as a real one is only past about fifteen honest long
    // sessions.
    const usedUp = await postAsPage(offline, `/api/v1/artifacts/${artifactId}/_test/used-up`);
    expect(usedUp.status(), await usedUp.text()).toBe(204);

    // Alice goes offline and pastes at the end of the meeting point's text: one edit, one saved
    // row, made while the server holds nothing after that place.
    await openSpec(offline, issue.key, MEETING_POINT);
    transport.hold();
    await transport.sever();
    await placeCaret(offline, "after", MEETING_POINT);
    await paste(offline, { html: "", text: "offline paste" });
    await expect.poll(() => pendingEditCount(offline, artifactId)).toBe(1);
    await offline.close();

    // Bob types in place there, each keystroke before the last: typing goes on in a used-up room.
    // His page stays open through the rebuild below, so it holds the history the rebuild replaces.
    const typing = await bob.newPage();
    const bobSockets = await recordDocumentSockets(typing);
    await openSpec(typing, issue.key, MEETING_POINT);
    await placeCaret(typing, "after", MEETING_POINT);
    for (const character of TYPED_IN_PLACE) {
      await typing.keyboard.type(character);
      await typing.keyboard.press("ArrowLeft");
    }
    const typedInPlace = [...TYPED_IN_PLACE].reverse().join("");
    await expect
      .poll(async () => (await getArtifactText(artifactId)).markdown)
      .toContain(`${MEETING_POINT}${typedInPlace}`);

    // Alice comes back: her saved paste merges past every one of Bob's keystrokes, a larger apply
    // than the used-up room takes, so it is dropped with its reason and the rebuild is offered.
    const page = await alice.newPage();
    const sockets = await recordDocumentSockets(page);
    await page.goto(`/issues/${issue.key}/spec`);
    await expect(page.getByText(NEEDS_REBUILD)).toBeVisible();
    const offer = page.getByRole("button", { name: "Rebuild this document" });
    await expect(offer).toBeVisible();
    await expect(connectedDot(page)).toHaveText("connected");
    // Bob's cursor label sits between the meeting point and his keystrokes in Alice's editor.
    await expect(documentEditor(page)).toContainText(typedInPlace);
    await expect(documentEditor(page)).not.toContainText("offline paste");
    await expect.poll(() => pendingEditCount(page, artifactId)).toBe(0);
    expect((await getArtifactText(artifactId)).markdown).not.toContain("offline paste");

    let confirmation: string | undefined;
    page.once("dialog", (dialog) => {
      confirmation = dialog.message();
      void dialog.accept();
    });
    // Each browser's document sockets from here on are the ones that can meet the rebuilt room.
    const beforeRebuild = {
      alice: (await sockets()).opened,
      bob: (await bobSockets()).opened,
    };
    await offer.click();
    await expect(page.getByText(NEEDS_REBUILD)).toHaveCount(0);
    expect(confirmation).toContain(
      "Every browser connected to it, this one included, drops the edits it has not sent yet."
    );
    await expect(connectedDot(page)).toHaveText("connected");
    await expect(documentEditor(page)).toContainText(typedInPlace);

    await typeAtEnd(page, "saved after the rebuild");
    await expect
      .poll(async () => (await getArtifactText(artifactId)).markdown)
      .toContain("saved after the rebuild");
    await expect.poll(() => pendingEditCount(page, artifactId)).toBe(0);
    // One refusal: the editor closed its connection before the rebuild, so it handed the replaced
    // history to no rebuilt room.
    expect((await sockets()).closes.filter((code) => code === 4413)).toHaveLength(1);

    // Bob's page held the replaced history when the rebuild closed its socket. Its reconnect hands
    // that history to the rebuilt room once, the room refuses it, and Bob is told, reads the
    // rebuilt document afresh and sees Alice's edit; no later socket of his carries it again.
    await expect(typing.getByText(REBUILT_ELSEWHERE)).toBeVisible();
    await expect(connectedDot(typing)).toHaveText("connected");
    await expect(documentEditor(typing)).toContainText("saved after the rebuild");
    // At most one DOC_REBUILT refusal for each browser of the row, counted from what each page
    // sent: Alice's none, and Bob's the one his reconnect met.
    const bobRecord = await bobSockets();
    expect(await replacedHistoryHandoffs(page, beforeRebuild.alice)).toEqual([]);
    expect(
      await replacedHistoryHandoffs(typing, beforeRebuild.bob),
      `the sizes of the replaced histories Bob's page handed the rebuilt room; its document sockets closed with ${bobRecord.closes.join(", ")}`
    ).toHaveLength(1);
  } finally {
    await alice.close();
    await bob.close();
  }
});

test("a browser holding the history someone else's rebuild replaced is told the document was rebuilt, and offered no rebuild", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await createProject({ key: "REPLACED", name: "Replaced histories" });
  const issue = await createIssue({
    project: "REPLACED",
    spec: `${MEETING_POINT}\n\nThe tangle follows.\n`,
    title: "Read a document someone else rebuilt",
  });
  const artifactId = issue.primary_artifact_id;
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");
  try {
    const page = await alice.newPage();
    const transport = await documentTransport(page);
    const sockets = await recordDocumentSockets(page);
    // The same hook the row above uses: the document's stored history is used up, so Bob's
    // rebuild of it is taken (docs.nearACap).
    const usedUp = await postAsPage(page, `/api/v1/artifacts/${artifactId}/_test/used-up`);
    expect(usedUp.status(), await usedUp.text()).toBe(204);
    await openSpec(page, issue.key, MEETING_POINT);

    // Alice goes offline and types: her browser holds the history with an edit on top of it, and
    // its reconnect waits until the rebuild has replaced that history.
    transport.hold();
    // Every document socket her page opens from here on is held until after the rebuild, so these
    // are the ones that can meet the rebuilt room.
    const beforeRebuild = (await sockets()).opened;
    await transport.sever();
    await typeAtEnd(page, "typed before the rebuild");
    await expect.poll(() => pendingEditCount(page, artifactId)).toBeGreaterThan(0);

    const bobPage = await bob.newPage();
    const rebuilt = await postAsPage(bobPage, `/api/v1/artifacts/${artifactId}/rebuild`, {});
    expect(rebuilt.status(), await rebuilt.text()).toBe(200);

    // Alice's socket reconnects and hands the replaced history back, which the rebuilt room
    // refuses: she is told the document was rebuilt, her unsent edit is gone, and no rebuild is
    // offered, since the route would answer a second rebuild of it 409.
    await transport.release();
    await expect(page.getByText(REBUILT_ELSEWHERE)).toBeVisible();
    await expect(page.getByRole("button", { name: "Rebuild this document" })).toHaveCount(0);
    // The room answered each frame she typed offline not applied, since it parks behind the
    // history the rebuild replaced, so her saved rows outlive the refusal. The document read
    // afresh drops them as rows typed against a rebuilt history, and its connection dot says when
    // they were saved (pending-sync.ts `parkedClients`, connection.ts `pendingNotice`): the dot
    // reads "connected" alone only from the new socket's first frame until that restore reports.
    await expect(connectedDot(page)).toHaveAccessibleName(
      /^connected · Edits saved in this browser at .+ could not be applied: the document was rebuilt since\.$/u
    );
    await expect(documentEditor(page)).toContainText(MEETING_POINT);
    await expect(documentEditor(page)).not.toContainText("typed before the rebuild");
    await expect.poll(() => pendingEditCount(page, artifactId)).toBe(0);
    expect((await getArtifactText(artifactId)).markdown).not.toContain("typed before the rebuild");

    // The reloaded document takes her next edit.
    await typeAtEnd(page, "typed after the rebuild");
    await expect
      .poll(async () => (await getArtifactText(artifactId)).markdown)
      .toContain("typed after the rebuild");
    await expect.poll(() => pendingEditCount(page, artifactId)).toBe(0);
    // One refusal, and no reconnect that met it again: the editor read the rebuilt document
    // rather than hand the replaced history back once more. Counted from what Alice's page sent,
    // not from its 4413 closes: the room's refusal can reach the page as an abnormal closure (1006)
    // with no reason, and a count of 4413s misses that close and any history handed over after it.
    // Bob's page opens no document, so it has none to count.
    const record = await sockets();
    expect(
      await replacedHistoryHandoffs(page, beforeRebuild),
      `the sizes of the replaced histories Alice's page handed the rebuilt room; its document sockets closed with ${record.closes.join(", ")}`
    ).toHaveLength(1);
  } finally {
    await alice.close();
    await bob.close();
  }
});

// The person who opens a document an agent's writes have carried near the decode cap meets no
// refusal of their own, and the rebuild route is a person's alone, so unless the page offers it
// nobody can run it. The document's read says the route takes it (near_decode_cap, the route's
// own test), so the page offers it at once with the server's reason; the rebuild leaves the
// document small, its text whole, its editor taking edits and the refused write landing.
// SLOW(>=2s): the hook writes a million deleted characters into the stored history, the page's
// browser is handed that history, and the rebuild loads and replaces it.
test("a document near the decode cap offers its rebuild to a person who met no refusal, and takes edits after it", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await createProject({ key: "NEARCAP", name: "Documents near the decode cap" });
  // Four top-level blocks, so the hook leaves the document where a keystroke still lands and a
  // larger write does not: the state a person meets after an agent's rewrites were refused.
  const issue = await createIssue({
    project: "NEARCAP",
    spec: `${MEETING_POINT}\n\nAn agent rewrote this document many times.\n\nIts history holds every rewrite.\n\nIts text is four short paragraphs.\n`,
    title: "Rebuild a document near the decode cap",
  });
  const artifactId = issue.primary_artifact_id;
  /** The agent's next version: ten paragraphs more, far more structs than the cap has room for. */
  const nextVersion = {
    ops: [
      {
        after: "end",
        markdown: Array.from(
          { length: 10 },
          (_, index) => `The agent's paragraph ${index}.\n`
        ).join("\n"),
        op: "insert" as const,
      },
    ],
  };
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // The hook appends to the stored history one update of deleted characters that carries the
    // document just past the band its read answers near for, as a history of many whole-text
    // rewrites does, and leaves its text as it was.
    const nearCap = await postAsPage(page, `/api/v1/artifacts/${artifactId}/_test/near-decode-cap`);
    expect(nearCap.status(), await nearCap.text()).toBe(204);
    await expect(editArtifact(artifactId, nextVersion)).rejects.toThrow(
      "a person can rebuild the document"
    );
    const read = await dashboardTextRead(page, artifactId);
    expect(read.near_decode_cap).toBe(true);
    const reason = read.near_decode_cap_reason ?? "";
    expect(reason).not.toBe("");

    // The offer comes from the page's read of the text, before the editor has drawn the million
    // structs the browser is handed: the person need not wait for those to be offered the remedy.
    await page.goto(`/issues/${issue.key}/spec`);
    await expect(page.getByText(NEAR_CAP)).toBeVisible();
    await expect(page.getByText(reason)).toBeVisible();
    await expect(page.getByText(NEEDS_REBUILD)).toHaveCount(0);
    const offer = page.getByRole("button", { name: "Rebuild this document" });
    await expect(offer).toBeVisible();

    let confirmation: string | undefined;
    page.once("dialog", (dialog) => {
      confirmation = dialog.message();
      void dialog.accept();
    });
    await offer.click();
    await expect(page.getByText(NEAR_CAP)).toHaveCount(0);
    await expect(offer).toHaveCount(0);
    expect(confirmation).toContain(
      "Every browser connected to it, this one included, drops the edits it has not sent yet."
    );
    expect((await dashboardTextRead(page, artifactId)).near_decode_cap).toBe(false);
    await expect(connectedDot(page)).toHaveText("connected");
    await expect(documentEditor(page)).toContainText("Its text is four short paragraphs.");

    await typeAtEnd(page, "saved after the rebuild");
    await expect
      .poll(async () => (await getArtifactText(artifactId)).markdown)
      .toContain("saved after the rebuild");
    await editArtifact(artifactId, nextVersion);
    expect((await getArtifactText(artifactId)).markdown).toContain("The agent's paragraph 9.");
  } finally {
    await alice.close();
  }
});

// A document far from the cap is one a write can still change, and the route refuses to rebuild
// it: its read says so, and the page offers no rebuild.
// SLOW(>=2s): opens the document's page and waits for its editor to draw the text the read that
// decides the offer answered.
test("a document far from the decode cap offers no rebuild", async ({ browser }) => {
  await createProject({ key: "FARCAP", name: "Documents far from the decode cap" });
  const issue = await createIssue({
    project: "FARCAP",
    spec: `${MEETING_POINT}\n`,
    title: "No rebuild for a document far from the cap",
  });
  const artifactId = issue.primary_artifact_id;
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    expect((await dashboardTextRead(page, artifactId)).near_decode_cap).toBe(false);
    // The editor draws only after the page's read of the text, which is the read the offer
    // would come from: once the text shows, an offer that was coming would be on the page.
    await page.goto(`/issues/${issue.key}/spec`);
    await expect(documentEditor(page)).toContainText(MEETING_POINT);
    await expect(page.getByText(NEAR_CAP)).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Rebuild this document" })).toHaveCount(0);
    const rebuilt = await postAsPage(page, `/api/v1/artifacts/${artifactId}/rebuild`, {});
    expect(rebuilt.status(), await rebuilt.text()).toBe(409);
  } finally {
    await alice.close();
  }
});

test("a merge the server cannot scan through is dropped with its reason, and the next edit saves", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await createProject({ key: "TANGLE", name: "Tangled documents" });
  const issue = await createIssue({
    project: "TANGLE",
    spec: "# Tangled\n\nThe tangle follows the next paragraph.\n",
    title: "Refuse a merge that scans too far",
  });
  const artifactId = issue.primary_artifact_id;
  const alice = await asUser(browser, "alice");
  try {
    const offline = await alice.newPage();
    const transport = await documentTransport(offline);
    await openSpec(offline, issue.key, "The tangle follows the next paragraph.");
    await typeAtEnd(offline, TANGLE_POINT);
    await expect
      .poll(async () => (await getArtifactText(artifactId)).markdown)
      .toContain(TANGLE_POINT);
    await expect.poll(() => pendingEditCount(offline, artifactId)).toBe(0);

    // Alice goes offline and pastes at the end of what she typed: one edit, one saved row, made
    // while the server holds nothing after that place, so it names that place's last character as
    // its left origin and nothing as its right.
    transport.hold();
    await transport.sever();
    await placeCaret(offline, "after", TANGLE_POINT);
    await paste(offline, { html: "", text: "tangled paste" });
    await expect.poll(() => pendingEditCount(offline, artifactId)).toBe(1);

    // The hook chains tens of thousands of items onto the end of that text
    // (docs.Service.TangleBeyondOneApplyForTest), each naming the place Alice's paste names: a
    // real document is tangled that far only where as many items were made at one place while a
    // browser was away, which no browser test can type. Merging her paste scans the chain to its
    // end, past the items one apply of hers is allowed (27,594 of them for her 36-byte frame,
    // scanCeiling), while the room keeps most of its 1,000,000-item scan total, which is what
    // makes this refusal the too-tangled one and not a rebuild's.
    const tangled = await postAsPage(offline, `/api/v1/artifacts/${artifactId}/_test/tangled`);
    expect(tangled.status(), await tangled.text()).toBe(204);
    await offline.close();

    // Alice comes back: her saved paste is sent, refused whole, and dropped with its reason. The
    // room hands her the chain first, as a series of bounded frames, which is work this change's
    // fork had to be made linear in the chain's length to do at all: before sjawhar/ygo
    // v1.55.1-sami.7 the split walked its clients in the order the encoder writes them, against a
    // chain whose dependencies run the other way, so this document's handover took about a
    // hundred seconds of a pod's CPU and no browser of it ever synced (crdt/split.go).
    const page = await alice.newPage();
    const sockets = await recordDocumentSockets(page);
    await page.goto(`/issues/${issue.key}/spec`);
    await expect(page.getByText(TOO_TANGLED)).toBeVisible();
    // Nothing to rebuild: the document is nowhere near a cap, only this one merge was too far.
    await expect(page.getByRole("button", { name: "Rebuild this document" })).toHaveCount(0);
    await expect(connectedDot(page)).toHaveText("connected");
    await expect(documentEditor(page)).toContainText(TANGLE_POINT);
    await expect(documentEditor(page)).not.toContainText("tangled paste");
    await expect.poll(() => pendingEditCount(page, artifactId)).toBe(0);
    const refused = (await getArtifactText(artifactId)).markdown;
    expect(refused).not.toContain("tangled paste");
    expect(refused).toContain(TANGLE_POINT);

    await typeAtEnd(page, "saved after the refusal");
    await expect
      .poll(async () => (await getArtifactText(artifactId)).markdown)
      .toContain("saved after the refusal");
    await expect.poll(() => pendingEditCount(page, artifactId)).toBe(0);
    // One refusal, and no reconnect that met it again.
    expect((await sockets()).closes.filter((code) => code === 4413)).toHaveLength(1);
    expect((await getArtifactText(artifactId)).markdown).not.toContain("tangled paste");
  } finally {
    await alice.close();
  }
});

test("an edit holding content the document does not take is dropped with its reason, and the next edit saves", async ({
  browser,
}) => {
  await createProject({ key: "REFUSE", name: "Refused updates" });
  const issue = await createIssue({
    project: "REFUSE",
    spec: "# Refused\n\nOne paragraph of ordinary prose.\n",
    title: "Refuse an edit the document does not take",
  });
  const artifactId = issue.primary_artifact_id;
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const sockets = await recordDocumentSockets(page);
    await openSpec(page, issue.key, "One paragraph of ordinary prose.");
    await typeAtEnd(page, "kept before the refusal");
    await expect
      .poll(async () => (await getArtifactText(artifactId)).markdown)
      .toContain("kept before the refusal");
    await expect.poll(() => pendingEditCount(page, artifactId)).toBe(0);

    // The editor never sends content the server refuses to read - a subdocument, a root outside
    // the document's tree and its margin, bytes its decoder refuses - so the hook arms the room
    // to refuse the next update this browser sends it as it refuses one of those
    // (docs.Service.RefuseNextPeerUpdateForTest). A paste is one update, so it is that one.
    const armed = await postAsPage(
      page,
      `/api/v1/artifacts/${artifactId}/_test/refuse-next-update`
    );
    expect(armed.status(), await armed.text()).toBe(204);
    await paste(page, { html: "", text: "refused content" });

    await expect(page.getByText(UPDATE_REFUSED)).toBeVisible();
    // Permanent, as the size refusal is: no rebuild is offered, the refused edit is dropped, and
    // the editor is connected again against a fresh read.
    await expect(page.getByRole("button", { name: "Rebuild this document" })).toHaveCount(0);
    await expect(connectedDot(page)).toHaveText("connected");
    await expect(documentEditor(page)).toContainText("kept before the refusal");
    await expect(documentEditor(page)).not.toContainText("refused content");
    await expect.poll(() => pendingEditCount(page, artifactId)).toBe(0);
    expect((await getArtifactText(artifactId)).markdown).not.toContain("refused content");

    await typeAtEnd(page, "saved after the refusal");
    await expect
      .poll(async () => (await getArtifactText(artifactId)).markdown)
      .toContain("saved after the refusal");
    await expect.poll(() => pendingEditCount(page, artifactId)).toBe(0);
    // One refusal, and no reconnect that met it again.
    expect((await sockets()).closes.filter((code) => code === 4413)).toHaveLength(1);
    expect((await getArtifactText(artifactId)).markdown).not.toContain("refused content");
  } finally {
    await alice.close();
  }
});
