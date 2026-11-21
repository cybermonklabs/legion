import { afterEach, expect, jest, spyOn, test } from "bun:test";
import {
  DOCUMENT_BUSY_CLOSE_CODE,
  DOCUMENT_CLIENT_ID_MESSAGE_PREFIX,
  DOCUMENT_CLIENT_ID_PARAMETER,
  DOCUMENT_CLIENT_ID_REFUSED_CLOSE_REASON,
  DOCUMENT_REBUILT_CLOSE_REASON,
  DOCUMENT_TOO_LARGE_CLOSE_CODE,
} from "@legion/contracts";
import { waitFor } from "@testing-library/react";
import { createDecoder, readVarString, readVarUint, readVarUint8Array } from "lib0/decoding";
import * as Y from "yjs";

import { quietConsoleError } from "../../__tests__/act-warnings";
import {
  type ConnectionState,
  colorForLogin,
  connectionLabel,
  type DocumentConnection,
  isSchemaReadOnly,
  loadDocumentTransport,
  pendingNotice,
  type ReconnectWait,
  reconnectingNotice,
  refusedEditNotice,
  wsUrl,
} from "./connection";
import {
  artifactId,
  authenticatedFrame,
  awarenessFrame,
  closeStores,
  frame,
  paragraphText,
  rebuiltRoom,
  rowsOf,
  savedChanges,
  savedEdit,
  serverWith,
  serverWithHeading,
  stampHeadingId,
  storeFor,
  syncStatusFrame,
  syncStep1Frame,
  syncStep2Frame,
  typeInto,
  typeParagraph,
  varBytes,
} from "./pending-fixture";
import * as pendingSyncModule from "./pending-sync";
import { type PendingState, startPendingSync } from "./pending-sync";

afterEach(closeStores);

test("wsUrl builds an encoded same-origin document websocket URL", () => {
  const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
  expect(wsUrl("artifact / one")).toBe(
    `${protocol}//${window.location.host}/ws/doc/artifact%20%2F%20one`
  );
});

test("colorForLogin assigns each login a stable, valid CSS presence color", () => {
  for (const login of ["alice", "bob"]) {
    const color = colorForLogin(login);
    expect(color).toBe(colorForLogin(login));
    expect(CSS.supports("color", color)).toBe(true);
  }
});

test("a read-only Hocuspocus admission signals a schema reload", () => {
  expect(isSchemaReadOnly("readonly")).toBe(true);
  expect(isSchemaReadOnly("read-write")).toBe(false);
  expect(isSchemaReadOnly(undefined)).toBe(false);
});

test("pending document labels describe retained, read-only, rebuilt, unsent, and unretained edits", () => {
  const retained = {
    count: 2,
    readOnly: false,
    rebuiltAt: undefined,
    stored: true,
    unsent: false,
  };
  expect(connectionLabel("offline", pendingNotice("offline", retained))).toBe(
    "offline · 2 edits saved in this browser, not sent yet."
  );
  expect(pendingNotice("connected", { ...retained, readOnly: true })).toBe(
    "2 edits saved in this browser can't be sent: this document is read-only."
  );
  expect(pendingNotice("connecting", { ...retained, stored: false })).toBe(
    "Edits typed now will not survive a reload."
  );
  expect(pendingNotice("connected", { ...retained, rebuiltAt: 0 })).toBe(
    `Edits saved in this browser at ${new Date(0).toLocaleString()} could not be applied: the document was rebuilt since.`
  );
  expect(pendingNotice("connected", retained)).toBeUndefined();
  expect(connectionLabel("connected", pendingNotice("connected", retained))).toBe("connected");
  expect(pendingNotice("offline", { ...retained, count: 1 })).toBe(
    "1 edit saved in this browser, not sent yet."
  );
  // Rows that have waited out `UNSENT_NOTICE_MS` on a connected, writable socket are named; the
  // same rows inside that wait are not, which is what keeps the sentence off ordinary typing.
  const unsent = { ...retained, unsent: true };
  expect(pendingNotice("connected", unsent)).toBe(
    "2 edits saved in this browser, not sent to Dispatch yet. Dispatch keeps trying."
  );
  expect(connectionLabel("connected", pendingNotice("connected", unsent))).toBe(
    "connected · 2 edits saved in this browser, not sent to Dispatch yet. Dispatch keeps trying."
  );
  expect(pendingNotice("connected", { ...unsent, count: 1 })).toBe(
    "1 edit saved in this browser, not sent to Dispatch yet. Dispatch keeps trying."
  );
  // The read-only sentence and the offline one are unchanged by it, and rows that have landed
  // are named by nothing.
  expect(pendingNotice("connected", { ...unsent, readOnly: true })).toBe(
    "2 edits saved in this browser can't be sent: this document is read-only."
  );
  expect(pendingNotice("offline", unsent)).toBe("2 edits saved in this browser, not sent yet.");
  expect(pendingNotice("connected", { ...unsent, count: 0 })).toBeUndefined();
});

/** A socket already open, as the browser's is once it connects; the test plays the server. */
class OpenSocket extends EventTarget {
  static opened: OpenSocket[] = [];
  binaryType = "blob";
  readonly readyState: number = 1;
  readonly sent: Uint8Array[] = [];

  constructor(readonly url: string) {
    super();
    OpenSocket.opened.push(this);
    queueMicrotask(() => this.dispatchEvent(new Event("open")));
  }

  /** Whether the client closed this socket. */
  closed = false;

  close(): void {
    this.closed = true;
  }

  send(data: Uint8Array): void {
    this.sent.push(new Uint8Array(data));
  }

  receive(data: Uint8Array): void {
    this.dispatchEvent(new MessageEvent("message", { data: data.slice().buffer }));
  }

  /** The server closes the socket, as a dropped connection does; the provider reconnects. */
  drop(): void {
    this.dispatchEvent(new CloseEvent("close", { code: 1006 }));
  }
}

/**
 * A socket whose own `close()` reaches the provider, as a browser's does once it has closed the
 * connection: what `DocumentConnection.readmit` relies on to be admitted again.
 */
class ClosingSocket extends OpenSocket {
  /** How many times the page closed this socket: the provider closes one again as it tidies it. */
  closes = 0;

  override close(): void {
    this.closes += 1;
    queueMicrotask(() => this.dispatchEvent(new CloseEvent("close", { code: 1000 })));
  }
}

interface ProviderHarness {
  connection: DocumentConnection;
  /** Each admission the host was told of, read-only or not, in order. */
  admissions: boolean[];
  /** Each pending state the host was told of, in order. */
  pending: PendingState[];
  /** Each close for want of room the host was told of, `true` as it closes and `false` once a
   *  frame of this browser's is applied again (`DOCUMENT_BUSY_CLOSE_CODE`). */
  busy: boolean[];
  /** The reason of each refused edit the host was told of (`DOCUMENT_TOO_LARGE_CLOSE_CODE`). */
  refusals: string[];
  /** How many saved edits went with each refused edit, where the connection said. */
  dropped: (number | undefined)[];
  /** Each reconnect wait the host was told of, and each `undefined` taking one back, in order. */
  waits: (ReconnectWait | undefined)[];
  /** Whether the host was told that the first admission's restore finished. */
  synced(): boolean;
  /** The `index`th socket the provider opened, once the provider has written to it. */
  socket(index: number): Promise<OpenSocket>;
  /** Sets the share of the reconnect spread (`Math.random()`) each socket draws when it opens. */
  draw(share: number): void;
}

/**
 * Connects `artifact` through the real transport and Hocuspocus provider, over `socketClass`
 * sockets the test answers as the server, runs `body` with what the host is told, and then
 * destroys the connection and puts the browser's WebSocket and `Math.random` back. Each socket
 * draws `share` of the reconnect spread when it opens, 0 unless the test says otherwise, so the
 * reconnect after a dropped socket opens at once.
 */
async function withProvider(
  artifact: string,
  body: (harness: ProviderHarness) => Promise<void>,
  share = 0,
  socketClass: typeof OpenSocket = OpenSocket
): Promise<void> {
  const realSocket = globalThis.WebSocket;
  globalThis.WebSocket = socketClass as unknown as typeof WebSocket;
  OpenSocket.opened = [];
  const random = spyOn(Math, "random").mockReturnValue(share);
  let connection: DocumentConnection | undefined;
  try {
    const connect = await loadDocumentTransport();
    let synced = false;
    const admissions: boolean[] = [];
    const pending: PendingState[] = [];
    const busy: boolean[] = [];
    const refusals: string[] = [];
    const dropped: (number | undefined)[] = [];
    const waits: (ReconnectWait | undefined)[] = [];
    connection = connect(artifact, {
      onAdmission: (readOnly) => admissions.push(readOnly),
      onBusy: (waiting) => busy.push(waiting),
      onEditRefused: (reason, count) => {
        refusals.push(reason);
        dropped.push(count);
      },
      onOutsideSchema() {},
      onPending: (state) => pending.push(state),
      onReconnectWait: (wait) => waits.push(wait),
      onStatus() {},
      onSynced: () => {
        synced = true;
      },
      schemaCreatable: 1,
      schemaVersion: 1,
    });
    await body({
      admissions,
      busy,
      connection,
      draw: (next) => random.mockReturnValue(next),
      dropped,
      pending,
      refusals,
      async socket(index) {
        await waitFor(() => expect(OpenSocket.opened[index]?.sent.length).toBeGreaterThan(0));
        return OpenSocket.opened[index] as OpenSocket;
      },
      synced: () => synced,
      waits,
    });
  } finally {
    connection?.destroy();
    random.mockRestore();
    globalThis.WebSocket = realSocket;
  }
}

/** The payloads of the Sync frames of `step` (0 SyncStep1, 1 SyncStep2, 2 Update) a socket wrote. */
function syncPayloads(socket: OpenSocket, name: string, step: number): Uint8Array[] {
  const prefix = frame(name, [0, step]);
  return socket.sent
    .filter((written) => prefix.every((byte, index) => written[index] === byte))
    .map((written) => {
      const decoder = createDecoder(written);
      readVarString(decoder); // the document name
      readVarUint(decoder); // Sync
      readVarUint(decoder); // the step
      return readVarUint8Array(decoder);
    });
}

/**
 * Answers a socket that just opened in the order Dispatch's server (ygo) does. ygo queues its
 * SyncStep1, its SyncStep2 and the room's awareness before its read loop starts, and only that
 * loop reads the socket's token, so the socket is synced before it is authenticated. The loop then
 * answers the socket's own SyncStep1 with a SyncStep2, and the SyncStep2 the provider wrote in
 * answer to the server's SyncStep1 with a SyncStatus: a writable admission applies it to `server`
 * and answers 1; a read-only one applies nothing and answers 1 only when `server` already holds
 * every struct in it. `beforeAuthentication` runs once the awareness has arrived, before the
 * socket is authenticated: the span in which the admission is not known yet.
 */
function serveInYgoOrder(
  socket: OpenSocket,
  server: Y.Doc,
  scope: "readonly" | "read-write",
  name: string,
  beforeAuthentication: () => void = () => {}
): void {
  socket.receive(syncStep1Frame(Y.encodeStateVector(server), name));
  socket.receive(syncStep2Frame(Y.encodeStateAsUpdate(server), name));
  socket.receive(awarenessFrame(name));
  beforeAuthentication();
  socket.receive(authenticatedFrame(scope, name));
  socket.receive(syncStep2Frame(Y.encodeStateAsUpdate(server), name));
  const answer = syncPayloads(socket, name, 1).at(-1);
  if (answer === undefined) {
    throw new Error("the provider answers the server's SyncStep1 with a SyncStep2");
  }
  if (scope === "readonly") {
    const missing = Y.diffUpdate(answer, Y.encodeStateVector(server));
    socket.receive(syncStatusFrame(Y.decodeUpdate(missing).structs.length === 0, name));
    return;
  }
  Y.applyUpdate(server, answer);
  socket.receive(syncStatusFrame(true, name));
}

/** Applies every Update frame a socket wrote to `server`, and acknowledges each, as a writable ygo room does. */
function answerUpdates(socket: OpenSocket, server: Y.Doc, name: string): void {
  for (const update of syncPayloads(socket, name, 2)) {
    Y.applyUpdate(server, update);
    socket.receive(syncStatusFrame(true, name));
  }
}

/** Types `text` at the end of the paragraph after the heading. */
function typeAfterHeading(doc: Y.Doc, text: string): void {
  const paragraph = doc.getXmlFragment("prosemirror").get(1);
  const typed = paragraph instanceof Y.XmlElement ? paragraph.get(0) : undefined;
  if (!(typed instanceof Y.XmlText)) {
    throw new Error("the block after the heading is a paragraph of text");
  }
  typed.insert(typed.length, text);
}

test("a real provider's edit typed on an open socket is cleared by its own acknowledgement", async () => {
  const artifact = artifactId();
  await withProvider(artifact, async ({ connection, socket: opened, synced }) => {
    const socket = await opened(0);
    socket.receive(authenticatedFrame("read-write", artifact));
    // The provider answers the server's SyncStep1 with a SyncStep2 of its own: the first frame on
    // this socket the server acknowledges.
    socket.receive(syncStep1Frame(Uint8Array.of(0), artifact));
    socket.receive(syncStep2Frame(Y.encodeStateAsUpdate(new Y.Doc()), artifact));
    await waitFor(() => expect(synced()).toBe(true));
    socket.receive(syncStatusFrame(true, artifact));

    // On an open socket the provider writes each edit's frame inside the document's own update
    // event, before any later listener has stored the edit.
    typeInto(connection.doc, "first");
    typeInto(connection.doc, " second");
    socket.receive(syncStatusFrame(true, artifact));

    const clientId = connection.doc.clientID;
    await waitFor(async () => expect(await rowsOf(artifact)).toEqual([[artifact, clientId, 2]]));
  });
});

test("a reconnect's writable admission restores another tab's row only once that reconnect's sync has applied", async () => {
  const artifact = artifactId();
  const server = serverWith("shared", 100);
  const firstState = Y.encodeStateAsUpdate(server);
  // While this tab is admitted read-only, the issue reopens and a person types into the paragraph.
  // Another tab types after that text and closes before its socket sends the edit, so its row is
  // the edit's only copy, and it builds on an update this tab has not received yet.
  const person = new Y.Doc();
  person.clientID = 200;
  Y.applyUpdate(person, firstState);
  typeInto(person, " added");
  Y.applyUpdate(server, Y.encodeStateAsUpdate(person));
  await savedEdit(artifact, server, " typed");

  // Each restore the transport starts for the reconnect, with whether the reconnect's SyncStep2
  // had reached the socket by then.
  const reconnectRestores: boolean[] = [];
  const running: Promise<void>[] = [];
  let syncDelivered = false;
  // The transport imports the pending sync itself, so its sync is wrapped where it is started.
  const realStart = startPendingSync;
  const started = spyOn(pendingSyncModule, "startPendingSync").mockImplementation((options) => {
    const sync = realStart(options);
    return {
      ...sync,
      synced() {
        if (OpenSocket.opened.length > 1) {
          reconnectRestores.push(syncDelivered);
        }
        const restoring = sync.synced();
        running.push(restoring);
        return restoring;
      },
    };
  });
  try {
    await withProvider(artifact, async ({ connection, pending, socket, synced }) => {
      const first = await socket(0);
      first.receive(authenticatedFrame("readonly", artifact));
      first.receive(syncStep2Frame(firstState, artifact));
      await waitFor(() => expect(synced()).toBe(true));
      await waitFor(() => expect(pending.at(-1)).toMatchObject({ count: 1, readOnly: true }));

      first.drop();
      const second = await socket(1);
      // A server that authenticates the reconnect before its SyncStep2 arrives: whatever the
      // authentication starts runs to its end before the sync is delivered.
      second.receive(authenticatedFrame("read-write", artifact));
      await Promise.all(running);
      syncDelivered = true;
      second.receive(syncStep2Frame(Y.encodeStateAsUpdate(server), artifact));

      const doc = connection.doc;
      await waitFor(() => expect(paragraphText(doc).toString()).toBe("shared added typed"));
      await Promise.all(running);
      await waitFor(async () =>
        expect(await rowsOf(artifact)).toEqual([[artifact, doc.clientID, 1]])
      );
      expect(pending.every((state) => state.rebuiltAt === undefined)).toBe(true);
      expect(reconnectRestores).toEqual([true]);
    });
  } finally {
    started.mockRestore();
  }
});

test("a reopened issue's read-only load stores nothing, and its writable reconnect restores another tab's row and stores what is typed after", async () => {
  const artifact = artifactId();
  const server = serverWithHeading(100);
  await withProvider(artifact, async ({ connection, pending, socket, synced }) => {
    const first = await socket(0);
    serveInYgoOrder(first, server, "readonly", artifact);
    await waitFor(() => expect(synced()).toBe(true));
    const doc = connection.doc;

    // The closed issue's editor stamps the heading's id as it loads, and the read-only room
    // refuses the provider's Update: a page that only loads names no edit.
    stampHeadingId(doc);
    first.receive(syncStatusFrame(false, artifact));
    await waitFor(() => expect(pending.at(-1)).toMatchObject({ count: 0, readOnly: true }));
    expect(await rowsOf(artifact)).toEqual([]);

    // While the page is open, the issue reopens and a person types a paragraph. Another tab adds
    // to it and closes before its socket sends the edit, so its row is the edit's only copy, and
    // it builds on an update this tab has not received yet.
    const person = new Y.Doc();
    person.clientID = 200;
    Y.applyUpdate(person, Y.encodeStateAsUpdate(server));
    typeParagraph(person);
    Y.applyUpdate(server, Y.encodeStateAsUpdate(person));
    await savedChanges(artifact, server, (browser) => typeAfterHeading(browser, " More."));

    first.drop();
    const second = await socket(1);
    serveInYgoOrder(second, server, "read-write", artifact);
    // The reconnect's SyncStep2 carried the stamp the read-only room refused.
    expect(server.getXmlFragment("prosemirror").toString()).toContain('id="plan"');

    // The other tab's row is restored against the synced document, and re-recorded under this
    // tab's client: none is dropped as built on a rebuilt history.
    await waitFor(() =>
      expect(doc.getXmlFragment("prosemirror").toString()).toContain("Typed. More.")
    );
    await waitFor(async () =>
      expect(await rowsOf(artifact)).toEqual([[artifact, doc.clientID, 1]])
    );

    typeAfterHeading(doc, " Mine.");
    await waitFor(async () =>
      expect(await rowsOf(artifact)).toEqual([1, 2].map((seq) => [artifact, doc.clientID, seq]))
    );
    await waitFor(() => expect(pending.at(-1)).toMatchObject({ count: 2, readOnly: false }));

    // The room applies and acknowledges the restored row's Update and the typing's: both clear.
    answerUpdates(second, server, artifact);
    await waitFor(async () => expect(await rowsOf(artifact)).toEqual([]));
    await waitFor(() => expect(pending.at(-1)).toMatchObject({ count: 0, readOnly: false }));
    expect(server.getXmlFragment("prosemirror").toString()).toContain("Typed. More. Mine.");
    expect(pending.every((state) => state.rebuiltAt === undefined)).toBe(true);
  });
});

// The server decides a socket's admission once, as it connects. A connection the host readmits
// closes its socket, the provider reconnects as it does after any close, and the new socket's
// admission is the one the host is told of.
test("a readmitted connection reports the admission of the socket it reconnects", async () => {
  const artifact = artifactId();
  const server = serverWithHeading(100);
  await withProvider(
    artifact,
    async ({ admissions, connection, socket, synced }) => {
      serveInYgoOrder(await socket(0), server, "readonly", artifact);
      await waitFor(() => expect(synced()).toBe(true));
      expect(admissions).toEqual([true]);

      connection.readmit();

      serveInYgoOrder(await socket(1), server, "read-write", artifact);
      await waitFor(() => expect(admissions).toEqual([true, false]));
      expect(OpenSocket.opened).toHaveLength(2);
    },
    0,
    ClosingSocket
  );
});

// The reopen's commit closes the room's sockets as the reopen answers the page, and the two race.
// A page that hears the answer last may have reconnected already, and the server admits that
// socket as the issue now is: the readmit leaves it to its own answer, so the reopen costs the
// page one reconnect and one draw, not two.
test("a readmit leaves the socket the server's close made the provider open while it waits for its answer", async () => {
  const artifact = artifactId();
  const server = serverWithHeading(100);
  await withProvider(
    artifact,
    async (harness) => {
      const { admissions, connection } = harness;
      const second = await loadReadOnlyThenDrop(harness, server, artifact);
      connection.readmit();
      expect(second.closes).toBe(0);

      serveInYgoOrder(second, server, "read-write", artifact);
      await waitFor(() => expect(admissions).toEqual([true, false]));
      expect(second.closes).toBe(0);
      expect(OpenSocket.opened).toHaveLength(2);
    },
    0,
    ClosingSocket
  );
});

test("a readmit leaves a socket the server has admitted writable", async () => {
  const artifact = artifactId();
  const server = serverWithHeading(100);
  await withProvider(
    artifact,
    async (harness) => {
      const { admissions, connection } = harness;
      const second = await loadReadOnlyThenDrop(harness, server, artifact);
      serveInYgoOrder(second, server, "read-write", artifact);
      await waitFor(() => expect(admissions).toEqual([true, false]));

      connection.readmit();

      expect(second.closes).toBe(0);
      expect(OpenSocket.opened).toHaveLength(2);
    },
    0,
    ClosingSocket
  );
});

// A socket the page opened before the reopen committed - after a blip, say - may be answered
// read-only after the page heard of the reopen: that answer is the closed issue's, so the readmit
// closes the socket as the answer arrives, and the host is told the admission of the next one.
test("a readmit closes the socket it found unanswered once that socket's answer is read-only", async () => {
  const artifact = artifactId();
  const server = serverWithHeading(100);
  await withProvider(
    artifact,
    async (harness) => {
      const { admissions, connection, socket } = harness;
      const second = await loadReadOnlyThenDrop(harness, server, artifact);
      connection.readmit();

      serveInYgoOrder(second, server, "readonly", artifact);
      expect(second.closes).toBe(1);

      serveInYgoOrder(await socket(2), server, "read-write", artifact);
      await waitFor(() => expect(admissions).toEqual([true, false]));
      expect(OpenSocket.opened).toHaveLength(3);
    },
    0,
    ClosingSocket
  );
});

// The socket a readmit found unanswered may drop before its answer: a blip, or the server's own
// room close after the socket's sync. The provider's next socket is judged by its own admission:
// a read-only one is reported and kept, as any socket's is, and the next readmit closes it.
test("a readmit judges the socket after one it found unanswered dropped by that socket's own admission", async () => {
  const artifact = artifactId();
  const server = serverWithHeading(100);
  await withProvider(
    artifact,
    async (harness) => {
      const { admissions, connection, socket } = harness;
      const second = await loadReadOnlyThenDrop(harness, server, artifact);
      connection.readmit();
      // ygo's frames ahead of the token's answer reach the socket, and it drops before that answer.
      second.receive(syncStep1Frame(Y.encodeStateVector(server), artifact));
      second.receive(syncStep2Frame(Y.encodeStateAsUpdate(server), artifact));
      second.receive(awarenessFrame(artifact));
      second.drop();

      const third = await closingSocket(harness, 2);
      serveInYgoOrder(third, server, "readonly", artifact);
      await waitFor(() => expect(admissions).toEqual([true, true]));
      expect(third.closes).toBe(0);

      connection.readmit();
      expect(third.closes).toBe(1);
      serveInYgoOrder(await socket(3), server, "read-write", artifact);
      await waitFor(() => expect(admissions).toEqual([true, true, false]));
      expect(OpenSocket.opened).toHaveLength(4);
    },
    0,
    ClosingSocket
  );
});

// The reopened issue's document: the reader's page was admitted read-only, so the rows it and
// other tabs saved were never sent. The readmission is writable, and its restore sends them.
test("a readmission that turns writable restores the rows the read-only admission kept", async () => {
  const artifact = artifactId();
  const server = serverWith("shared", 100);
  await savedEdit(artifact, server, " saved");
  await withProvider(
    artifact,
    async ({ connection, pending, socket, synced }) => {
      serveInYgoOrder(await socket(0), server, "readonly", artifact);
      await waitFor(() => expect(synced()).toBe(true));
      await waitFor(() => expect(pending.at(-1)).toMatchObject({ count: 1, readOnly: true }));
      expect(paragraphText(connection.doc).toString()).toBe("shared");

      connection.readmit();

      const second = await socket(1);
      serveInYgoOrder(second, server, "read-write", artifact);
      await waitFor(() => expect(paragraphText(connection.doc).toString()).toBe("shared saved"));
      await waitFor(() => expect(pending.at(-1)).toMatchObject({ count: 1, readOnly: false }));
      await waitFor(async () =>
        expect(await rowsOf(artifact)).toEqual([[artifact, connection.doc.clientID, 1]])
      );

      // The room applies and acknowledges the restored row: the browser keeps nothing unsent.
      answerUpdates(second, server, artifact);
      await waitFor(async () => expect(await rowsOf(artifact)).toEqual([]));
      // The host hears of the first admission's restore alone: no reload, no second mount.
      expect(synced()).toBe(true);
    },
    0,
    ClosingSocket
  );
});

test("a reopen and a second close name no unsent edit to a person who typed nothing", async () => {
  const artifact = artifactId();
  const server = serverWithHeading(100);
  await withProvider(artifact, async ({ connection, pending, socket, synced }) => {
    // No state the host was told of counts an edit or names one, whatever the connection shows.
    const expectNoNotice = async (readOnly: boolean) => {
      await waitFor(() => expect(pending.at(-1)).toMatchObject({ readOnly }));
      expect(await rowsOf(artifact)).toEqual([]);
      for (const state of pending) {
        expect(state.count).toBe(0);
        for (const shown of ["connecting", "connected", "offline"] satisfies ConnectionState[]) {
          expect(pendingNotice(shown, state)).toBeUndefined();
        }
      }
    };

    const first = await socket(0);
    serveInYgoOrder(first, server, "readonly", artifact);
    await waitFor(() => expect(synced()).toBe(true));
    // The closed issue's editor stamps the heading's id as it loads; the room refuses it.
    stampHeadingId(connection.doc);
    first.receive(syncStatusFrame(false, artifact));
    await expectNoNotice(true);

    // The issue reopens, and the socket reconnects writable.
    first.drop();
    const second = await socket(1);
    serveInYgoOrder(second, server, "read-write", artifact);
    expect(server.getXmlFragment("prosemirror").toString()).toContain('id="plan"');
    await expectNoNotice(false);

    // The issue closes again, and the socket reconnects read-only.
    second.drop();
    const third = await socket(2);
    serveInYgoOrder(third, server, "readonly", artifact);
    await expectNoNotice(true);
  });
});

/** The `index`th socket of a harness whose `withProvider` was handed `ClosingSocket`. */
async function closingSocket({ socket }: ProviderHarness, index: number): Promise<ClosingSocket> {
  const opened = await socket(index);
  if (!(opened instanceof ClosingSocket)) {
    throw new Error("the harness's sockets are ClosingSockets: withProvider was handed that class");
  }
  return opened;
}

/**
 * Loads `server` through a read-only admission, as a closed issue's page does, and drops the
 * socket, as the server's room close does. Returns the socket the provider reconnects with, once
 * it has written to it and before the server has answered it.
 */
async function loadReadOnlyThenDrop(
  harness: ProviderHarness,
  server: Y.Doc,
  artifact: string
): Promise<ClosingSocket> {
  const first = await harness.socket(0);
  serveInYgoOrder(first, server, "readonly", artifact);
  await waitFor(() => expect(harness.synced()).toBe(true));
  first.drop();
  return closingSocket(harness, 1);
}

/**
 * Loads `server`'s heading through a writable admission: the editor stamps the heading's id, and
 * the room takes the stamp and acknowledges it. The socket then drops, and while it is down an
 * agent's write leaves the heading's id empty again, as the server writes a heading, so the
 * reconnect's SyncStep2 brings an id for the editor to stamp anew.
 */
async function loadWritableThenDrop(
  { connection, pending, socket, synced }: ProviderHarness,
  server: Y.Doc,
  artifact: string
): Promise<void> {
  const first = await socket(0);
  serveInYgoOrder(first, server, "read-write", artifact);
  await waitFor(() => expect(synced()).toBe(true));
  stampHeadingId(connection.doc);
  answerUpdates(first, server, artifact);
  await waitFor(() => expect(pending.at(-1)).toMatchObject({ count: 0, readOnly: false }));
  expect(await rowsOf(artifact)).toEqual([]);

  first.drop();
  const heading = server.getXmlFragment("prosemirror").get(0);
  if (!(heading instanceof Y.XmlElement)) {
    throw new Error("the server's first block is a heading");
  }
  heading.setAttribute("id", "");
}

test("a writable page's reconnect to an issue closed meanwhile names no edit for a stamp its editor made before the admission", async () => {
  const artifact = artifactId();
  const server = serverWithHeading(100);
  await withProvider(artifact, async (harness) => {
    const { connection, pending, socket } = harness;
    await loadWritableThenDrop(harness, server, artifact);
    const reconnecting = pending.length;

    // The issue closed while the socket was down. The editor stamps the id the reconnect's
    // SyncStep2 brought before the read-only admission arrives, and the room refuses the stamp.
    const second = await socket(1);
    serveInYgoOrder(second, server, "readonly", artifact, () => stampHeadingId(connection.doc));
    second.receive(syncStatusFrame(false, artifact));

    await waitFor(() => expect(pending.at(-1)).toMatchObject({ readOnly: true }));
    expect(await rowsOf(artifact)).toEqual([]);
    for (const state of pending.slice(reconnecting)) {
      expect(state.count).toBe(0);
      for (const shown of ["connecting", "connected", "offline"] satisfies ConnectionState[]) {
        expect(pendingNotice(shown, state)).toBeUndefined();
      }
    }
  });
});

test("a writable reconnect never stores a stamp made before its admission, and the stamp's acknowledgement drops it", async () => {
  const artifact = artifactId();
  const server = serverWithHeading(100);
  await withProvider(artifact, async (harness) => {
    const { connection, pending, socket } = harness;
    await loadWritableThenDrop(harness, server, artifact);
    const doc = connection.doc;

    // The issue stays open: the stamp made before the admission is held, not stored.
    const second = await socket(1);
    serveInYgoOrder(second, server, "read-write", artifact, () => stampHeadingId(doc));
    await waitFor(() => expect(pending.at(-1)).toMatchObject({ readOnly: false }));
    expect(await rowsOf(artifact)).toEqual([]);

    // The room applies the stamp's own Update frame and acknowledges it, and nothing was counted.
    answerUpdates(second, server, artifact);
    expect(server.getXmlFragment("prosemirror").toString()).toContain('id="plan"');
    expect(await rowsOf(artifact)).toEqual([]);
    expect(pending.every((state) => state.count === 0)).toBe(true);
  });
});

test("a writable page stores the stamp it made offline ahead of what its person types, so a reload restores the typing", async () => {
  const artifact = artifactId();
  const server = serverWithHeading(100);
  await withProvider(artifact, async ({ connection, socket, synced }) => {
    const first = await socket(0);
    serveInYgoOrder(first, server, "read-write", artifact);
    await waitFor(() => expect(synced()).toBe(true));
    // The socket drops and nothing answers the reconnect. The editor stamps the heading's id, then
    // the person types: the typing continues the clock of the stamp, which the server never took.
    first.drop();
    const doc = connection.doc;
    stampHeadingId(doc);
    typeParagraph(doc);
    await waitFor(async () =>
      expect(await rowsOf(artifact)).toEqual([1, 2].map((seq) => [artifact, doc.clientID, seq]))
    );
  });

  // The reload: a new page, whose pending sync restores the rows of the same store.
  await withProvider(artifact, async ({ connection, pending, socket, synced }) => {
    serveInYgoOrder(await socket(0), server, "read-write", artifact);
    await waitFor(() => expect(synced()).toBe(true));
    expect(connection.doc.getXmlFragment("prosemirror").toString()).toContain("Typed.");
    expect(pending.every((state) => state.rebuiltAt === undefined)).toBe(true);
  });
});

test("a writable page's stamp the server acknowledges is never counted or stored, and logs no error", async () => {
  const artifact = artifactId();
  const server = serverWithHeading(100);
  const logged = spyOn(console, "error");
  try {
    await withProvider(artifact, async ({ connection, pending, socket, synced }) => {
      const first = await socket(0);
      serveInYgoOrder(first, server, "read-write", artifact);
      await waitFor(() => expect(synced()).toBe(true));
      const doc = connection.doc;
      stampHeadingId(doc);
      answerUpdates(first, server, artifact);
      expect(server.getXmlFragment("prosemirror").toString()).toContain('id="plan"');
      expect(await rowsOf(artifact)).toEqual([]);

      // What the person types next is stored alone: the acknowledged stamp left memory, so once
      // the room acknowledges the typing too, no row is left.
      typeParagraph(doc);
      await waitFor(async () =>
        expect(await rowsOf(artifact)).toEqual([[artifact, doc.clientID, 1]])
      );
      const typing = syncPayloads(first, artifact, 2).at(-1);
      if (typing === undefined) {
        throw new Error("the provider sends the typing in an Update frame");
      }
      Y.applyUpdate(server, typing);
      first.receive(syncStatusFrame(true, artifact));
      await waitFor(async () => expect(await rowsOf(artifact)).toEqual([]));
      await waitFor(() => expect(pending.at(-1)).toMatchObject({ count: 0, readOnly: false }));
      expect(pending.every((state) => state.count <= 1)).toBe(true);
    });
    expect(logged).not.toHaveBeenCalled();
  } finally {
    logged.mockRestore();
  }
});

test("a document whose browser keeps no edits says so, and still connects", async () => {
  const realStorage = globalThis.indexedDB;
  const logged = quietConsoleError();
  Object.defineProperty(globalThis, "indexedDB", { configurable: true, value: undefined });
  try {
    await withProvider(artifactId(), async ({ pending }) => {
      await waitFor(() => expect(OpenSocket.opened).toHaveLength(1));
      await waitFor(() =>
        expect(pending).toEqual([
          { count: 0, readOnly: false, rebuiltAt: undefined, stored: false, unsent: false },
        ])
      );
    });
  } finally {
    Object.defineProperty(globalThis, "indexedDB", { configurable: true, value: realStorage });
    logged.mockRestore();
  }
});

/** Lets the provider's promise chain (a socket's open, its retry's wait, then its attempt) run. */
async function settle(): Promise<void> {
  for (let i = 0; i < 10; i += 1) await Promise.resolve();
}

/**
 * Runs `body` over the real transport on a held clock (`jest.useFakeTimers()`), every draw of the
 * reconnect backoff half way unless `body` draws again. The page's first socket has been made by
 * then, and opened.
 */
async function onHeldClock(
  body: (held: { artifact: string; first: OpenSocket; harness: ProviderHarness }) => Promise<void>,
  socketClass: typeof OpenSocket = OpenSocket
): Promise<void> {
  const artifact = artifactId();
  try {
    await withProvider(
      artifact,
      async (harness) => {
        // The page's first connect waits nothing, whatever the draw would be.
        expect(OpenSocket.opened).toHaveLength(1);
        jest.useFakeTimers();
        harness.draw(0.5);
        await settle();
        await body({ artifact, first: OpenSocket.opened[0] as OpenSocket, harness });
      },
      0.999,
      socketClass
    );
  } finally {
    jest.useRealTimers();
  }
}

/**
 * Runs the held clock `wait` ms on and requires the provider's next socket to be made at that
 * moment and not a millisecond before it, then returns that socket, opened. The wait is part of
 * what each check compares, so a failure names the wait it was reading.
 */
async function nextSocketAfter(wait: number): Promise<OpenSocket> {
  const before = OpenSocket.opened.length;
  jest.advanceTimersByTime(wait - 1);
  await settle();
  expect({ opened: OpenSocket.opened.length - before, wait }).toEqual({ opened: 0, wait });
  jest.advanceTimersByTime(1);
  await settle();
  expect({ opened: OpenSocket.opened.length - before, wait }).toEqual({ opened: 1, wait });
  return OpenSocket.opened[before] as OpenSocket;
}

/** The server answers `socket`, which then stays up `upFor` ms before it drops as a reset drops it. */
async function answerThenDrop(socket: OpenSocket, artifact: string, upFor = 0): Promise<void> {
  socket.receive(authenticatedFrame("read-write", artifact));
  jest.advanceTimersByTime(upFor);
  socket.drop();
  await settle();
}

// A pod that admits the page and then dies, again and again (dispatch://INFRA-179): every socket
// is answered, then dropped. The provider's own backoff restarts at a socket's first frame, so it
// brought the page back within the first attempt's 2 s spread after every one of them, for ever.
// The connection's own backoff restarts only after a socket that stayed up 30 s. At half-way draws
// it waits the spread's 1 s, then 1.5, 3, 6 and 12 s, and 22.5 s from the 30 s cap on.
test("a document's first connect opens at once, and its reconnects after answered sockets back off until one stays up 30 s", async () => {
  await onHeldClock(async ({ artifact, first }) => {
    let socket = first;
    for (const wait of [1_000, 1_500, 3_000, 6_000, 12_000, 22_500, 22_500]) {
      await answerThenDrop(socket, artifact);
      socket = await nextSocketAfter(wait);
    }
    // A socket up a millisecond short of 30 s leaves the backoff at its cap...
    await answerThenDrop(socket, artifact, 29_999);
    socket = await nextSocketAfter(22_500);
    // ...and one up for 30 s starts it over: the spread, then the second attempt's 1.5 s.
    await answerThenDrop(socket, artifact, 30_000);
    socket = await nextSocketAfter(1_000);
    await answerThenDrop(socket, artifact);
    await nextSocketAfter(1_500);
  });
});

// A pod that is down: each socket after the first closes before the server answers it. The
// provider would try again on its own backoff, 1 s and then a draw between 1 s and a doubling
// ceiling: 1, 1.5 and 2.5 s at these draws. The connection cancels that retry and waits its own
// backoff instead, 1.5, 3 and 6 s, so one policy decides every wait.
test("a socket that closes before the server answers it waits the connection's backoff, not the provider's", async () => {
  await onHeldClock(async ({ artifact, first }) => {
    await answerThenDrop(first, artifact);
    let socket = await nextSocketAfter(1_000);
    for (const wait of [1_500, 3_000, 6_000]) {
      socket.drop();
      await settle();
      socket = await nextSocketAfter(wait);
    }
  });
});

// A busy close after the admission, as the server's refusal of the edit a full pod cannot take
// gives it: the edit waits for room, and the reconnects that carry it back off as any other.
test("busy closes after admissions back off, and the busy notice stays up across them", async () => {
  await onHeldClock(async ({ artifact, first, harness }) => {
    let socket = first;
    for (const wait of [1_000, 1_500, 3_000]) {
      socket.receive(authenticatedFrame("read-write", artifact));
      socket.dispatchEvent(
        new CloseEvent("close", { code: DOCUMENT_BUSY_CLOSE_CODE, reason: "DOC_BUSY" })
      );
      await settle();
      socket = await nextSocketAfter(wait);
    }
    expect(harness.busy).toEqual([true]);
  });
});

// A readmission is the page's own close, so it says nothing of the server: its reconnect waits
// the first attempt's spread, as it did before the connection kept a backoff of its own. A readmit
// closes only a socket admitted read-only (`readmit`), so the socket here is answered read-only.
test("a readmission reconnects within the first attempt's spread, whatever backoff earlier closes built", async () => {
  await onHeldClock(async ({ artifact, first, harness }) => {
    await answerThenDrop(first, artifact);
    let socket = await nextSocketAfter(1_000);
    await answerThenDrop(socket, artifact);
    socket = await nextSocketAfter(1_500);
    socket.receive(authenticatedFrame("readonly", artifact));
    harness.connection.readmit();
    await settle();
    await nextSocketAfter(1_000);
  }, ClosingSocket);
});

// The same holds where the readmit found its socket unanswered and the answer then came read-only:
// that close is the page's own as well, so it too waits the first attempt's spread, not the 3 s the
// two dropped sockets before it built.
test("a readmission whose socket answers read-only after the readmit reconnects within the first attempt's spread", async () => {
  await onHeldClock(async ({ artifact, first, harness }) => {
    await answerThenDrop(first, artifact);
    let socket = await nextSocketAfter(1_000);
    await answerThenDrop(socket, artifact);
    socket = await nextSocketAfter(1_500);
    harness.connection.readmit();
    socket.receive(authenticatedFrame("readonly", artifact));
    await settle();
    await nextSocketAfter(1_000);
  }, ClosingSocket);
});

// The provider stops for good after a close it takes as final: an unauthorized one, one forbidden
// and a message too big. The connection's backoff reconnects only where the provider would have.
for (const code of [4401, 4403, 1009]) {
  test(`a socket closed ${code} after its admission is not reconnected`, async () => {
    const warn = spyOn(console, "warn").mockImplementation(() => {});
    try {
      await onHeldClock(async ({ artifact, first }) => {
        first.receive(authenticatedFrame("read-write", artifact));
        first.dispatchEvent(new CloseEvent("close", { code }));
        await settle();
        jest.advanceTimersByTime(60_000);
        await settle();
        expect(OpenSocket.opened).toHaveLength(1);
      });
    } finally {
      warn.mockRestore();
    }
  });
}

/**
 * Requires the last wait the host was told of to be `attempt`'s and to end `inMs` from now. The
 * end is read against the clock rather than compared exactly: the held clock's `Date.now` still
 * takes the odd real millisecond, and a wait is a span to show a person, not a stamp.
 */
function expectWait(harness: ProviderHarness, attempt: number, inMs: number): void {
  const wait = harness.waits.at(-1);
  const left = wait === undefined ? undefined : wait.at - Date.now();
  expect({ attempt: wait?.attempt, left: Math.round((left ?? Number.NaN) / 100) * 100 }).toEqual({
    attempt,
    left: inMs,
  });
}

// What the page shows while a wait runs rests on being told of it: the wait, the attempt it is,
// and when it ends, taken back as the socket it waited for is made.
test("each reconnect wait is told to the host with its attempt and end, and taken back as its socket is made", async () => {
  await onHeldClock(async ({ artifact, first, harness }) => {
    await answerThenDrop(first, artifact);
    expect(harness.waits).toHaveLength(1);
    expectWait(harness, 0, 1_000);
    const second = await nextSocketAfter(1_000);
    expect(harness.waits).toHaveLength(2);
    expect(harness.waits.at(-1)).toBeUndefined();

    await answerThenDrop(second, artifact);
    expectWait(harness, 1, 1_500);
    await nextSocketAfter(1_500);
    expect(harness.waits.at(-1)).toBeUndefined();
  });
});

// The person who will not wait out a grown backoff presses Retry now. It sends the socket that
// wait was for and nothing else: the attempt was counted as the wait began, so a socket that
// closes again waits the attempt after it, and the growth the storm's bound rests on is kept.
test("reconnectNow sends the waiting socket at once and leaves the backoff where it stands", async () => {
  await onHeldClock(async ({ artifact, first, harness }) => {
    let socket = first;
    for (const wait of [1_000, 1_500, 3_000]) {
      await answerThenDrop(socket, artifact);
      socket = await nextSocketAfter(wait);
    }
    // The fourth wait is 6 s away; the person presses the button a second into it.
    await answerThenDrop(socket, artifact);
    expectWait(harness, 3, 6_000);
    jest.advanceTimersByTime(1_000);
    await settle();
    expect(OpenSocket.opened).toHaveLength(4);

    harness.connection.reconnectNow();
    jest.advanceTimersByTime(1);
    await settle();
    expect(OpenSocket.opened).toHaveLength(5);
    expect(harness.waits.at(-1)).toBeUndefined();
    socket = OpenSocket.opened[4] as OpenSocket;
    // The wait the press cut short opens nothing when its own 6 s would have been up.
    jest.advanceTimersByTime(6_000);
    await settle();
    expect(OpenSocket.opened).toHaveLength(5);

    // The socket it sent closes as the others did: the next wait is the attempt after it, 12 s,
    // not the 6 s this press spent and not the spread a reset would give.
    await answerThenDrop(socket, artifact);
    expectWait(harness, 4, 12_000);
    await nextSocketAfter(12_000);
  });
});

test("reconnectNow does nothing while a socket is open and after the connection is destroyed", async () => {
  await onHeldClock(async ({ artifact, first, harness }) => {
    // No wait is on: the socket is up, and the press opens no second socket.
    harness.connection.reconnectNow();
    await settle();
    expect(OpenSocket.opened).toHaveLength(1);

    await answerThenDrop(first, artifact);
    expectWait(harness, 0, 1_000);
    // A destroyed connection takes its wait back and opens nothing, pressed or not.
    harness.connection.destroy();
    expect(harness.waits.at(-1)).toBeUndefined();
    harness.connection.reconnectNow();
    jest.advanceTimersByTime(60_000);
    await settle();
    expect(OpenSocket.opened).toHaveLength(1);
  });
});

test("the reconnecting notice says what this browser keeps of the typing", () => {
  const pending = { count: 2, readOnly: false, rebuiltAt: undefined, stored: true, unsent: false };
  expect(reconnectingNotice(pending, 23)).toBe(
    "Reconnecting to the document in 23 s. Your edits are kept in this browser until they are sent."
  );
  expect(reconnectingNotice(pending)).toBe(
    "Reconnecting to the document. Your edits are kept in this browser until they are sent."
  );
  expect(reconnectingNotice({ ...pending, stored: false }, 4)).toBe(
    "Reconnecting to the document in 4 s. Your edits stay on this page, but a reload would lose them."
  );
  // A read-only admission takes no typing, and a document whose rows are not known yet promises
  // nothing of them.
  expect(reconnectingNotice({ ...pending, readOnly: true }, 4)).toBe(
    "Reconnecting to the document in 4 s."
  );
  expect(reconnectingNotice(undefined, 4)).toBe("Reconnecting to the document in 4 s.");
});

/**
 * Takes the provider's `index`th socket through a writable admission and the acknowledgement of
 * the SyncStep2 it wrote, which is what every refusal test below starts from.
 */
async function admitWritable(
  harness: ProviderHarness,
  artifact: string,
  index: number
): Promise<OpenSocket> {
  const socket = await harness.socket(index);
  socket.receive(authenticatedFrame("read-write", artifact));
  socket.receive(syncStep1Frame(Uint8Array.of(0), artifact));
  socket.receive(syncStep2Frame(Y.encodeStateAsUpdate(new Y.Doc()), artifact));
  await waitFor(() => expect(harness.synced()).toBe(true));
  socket.receive(syncStatusFrame(true, artifact));
  return socket;
}

for (const refusal of [
  {
    name: "too large",
    reason: "DOC_TOO_LARGE",
    notice:
      "Your last edit was not saved: it would make this document larger than a Dispatch document may be. Your other edits are kept.",
  },
  {
    name: "too tangled to merge",
    reason: "DOC_TOO_TANGLED",
    notice:
      "Your last edit was not saved: Dispatch could not merge it into this document quickly enough. Your other edits are kept.",
  },
  {
    name: "needing a rebuild",
    reason: "DOC_NEEDS_REBUILD",
    notice:
      "This document has been edited so much that it must be rebuilt before it can take this edit. Your other edits are kept.",
  },
  {
    name: "typed against a history a rebuild replaced",
    reason: "DOC_REBUILT",
    notice:
      "Someone else rebuilt this document, so the edits you had not sent could not be kept. Dispatch has reloaded the rebuilt document.",
  },
  {
    name: "holding content the document does not take",
    reason: "DOC_UPDATE_REFUSED",
    notice: "Your last edit was not saved: it holds content this document does not take.",
  },
]) {
  test(`a real provider closed as ${refusal.name} drops the refused edit's row and stops for the host`, async () => {
    const artifact = artifactId();
    await withProvider(artifact, async (harness) => {
      const socket = await admitWritable(harness, artifact, 0);
      const text = harness.connection.doc.getText("t");
      text.insert(0, "kept");
      text.insert(text.length, " refused");
      const clientId = harness.connection.doc.clientID;
      await waitFor(async () =>
        expect(await rowsOf(artifact)).toEqual([
          [artifact, clientId, 1],
          [artifact, clientId, 2],
        ])
      );
      socket.receive(syncStatusFrame(true, artifact));
      socket.receive(syncStatusFrame(false, artifact));
      socket.dispatchEvent(
        new CloseEvent("close", { code: DOCUMENT_TOO_LARGE_CLOSE_CODE, reason: refusal.reason })
      );

      await waitFor(() => expect(harness.refusals).toEqual([refusal.reason]));
      expect(refusedEditNotice(refusal.reason)).toBe(refusal.notice);
      expect(await rowsOf(artifact)).toEqual([]);
      // The provider's own close handler starts a reconnect before the connection's runs; the
      // connection's disconnect closes it, so no socket is left for the provider to sync on.
      expect(OpenSocket.opened.every((opened) => opened.closed)).toBe(true);
    });
  });
}

// The rebuilt room refuses the history it replaced and closes the socket at once, and the
// connection's reset can overtake that close: in Chromium the page then sees an abnormal closure
// with no reason, and at times without the refusal's SyncStatus either (document-refusals.e2e.ts's
// used-up row). The room's state reached the page before it, so the page knows its document holds
// that history, and a reconnect would hand it back to the same refusal.
test("a document holding the history a rebuild replaced is not reconnected when the room's refusal reaches it as an abnormal closure", async () => {
  const artifact = artifactId();
  const history = serverWith("before the rebuild", 100);
  await withProvider(artifact, async ({ refusals, socket, synced }) => {
    const first = await socket(0);
    serveInYgoOrder(first, history, "read-write", artifact);
    await waitFor(() => expect(synced()).toBe(true));

    // Someone else rebuilds the document: the room closes the page's socket, and the page's
    // reconnect meets the rebuilt room, whose SyncStep1 it answers with the history it holds.
    const rebuilt = rebuiltRoom(history, "after the rebuild", 300);
    first.drop();
    const second = await socket(1);
    second.receive(syncStep1Frame(Y.encodeStateVector(rebuilt), artifact));
    const handed = syncPayloads(second, artifact, 1).at(0);
    if (handed === undefined) {
      throw new Error("the provider answers the rebuilt room's SyncStep1 with a SyncStep2");
    }
    expect(Y.decodeUpdate(handed).structs.some(({ id }) => id.client === 100)).toBe(true);
    // The room sends its state, then refuses that answer, and the page sees the socket end with
    // no SyncStatus and no reason.
    second.receive(authenticatedFrame("read-write", artifact));
    second.receive(syncStep2Frame(Y.encodeStateAsUpdate(rebuilt), artifact));
    second.drop();

    await waitFor(() => expect(refusals).toEqual([DOCUMENT_REBUILT_CLOSE_REASON]));
    // The provider's own close handler starts a reconnect before the connection's runs; the
    // connection's disconnect closes it, so no socket is left to hand the history back on.
    expect(OpenSocket.opened.slice(2).every((opened) => opened.closed)).toBe(true);
  });
});

// A document the host built afresh from the rebuilt room holds the room's list of retired clients
// and none of their history, so a dropped socket is reconnected as any other. This row passes
// without the connection's check of that history too: it guards how far the check reaches, and a
// check that read the list alone (`doc.share.has("retired")`) fails it.
test("a document built afresh from a rebuilt room reconnects after its socket drops", async () => {
  const artifact = artifactId();
  const rebuilt = rebuiltRoom(serverWith("before the rebuild", 100), "after the rebuild", 300);
  await withProvider(artifact, async ({ refusals, socket, synced }) => {
    const first = await socket(0);
    serveInYgoOrder(first, rebuilt, "read-write", artifact);
    await waitFor(() => expect(synced()).toBe(true));

    first.drop();
    const second = await socket(1);
    expect(second.closed).toBe(false);
    expect(refusals).toEqual([]);
  });
});

// What document-refusals.e2e.ts's row of a browser holding the history someone else's rebuild
// replaced reads off the page. The page typed while its socket was down, and its reconnect meets
// the rebuilt room, which answers the typed frame not applied, since it parks behind the history
// the rebuild replaced, and refuses the SyncStep2 that hands that history back. The refusal drops
// no saved row, so the document the host reads afresh from the rebuilt room (ProofDocument's
// `onEditRefused`) drops them as rows typed against a rebuilt history, and its connection dot,
// once connected, says when they were saved.
test("edits a page typed against the history someone else's rebuild replaced are named as dropped by the document read afresh", async () => {
  const artifact = artifactId();
  const history = serverWith("before the rebuild", 100);
  const rebuilt = rebuiltRoom(history, "after the rebuild", 300);
  await withProvider(artifact, async ({ connection, refusals, socket, synced }) => {
    const first = await socket(0);
    serveInYgoOrder(first, history, "read-write", artifact);
    await waitFor(() => expect(synced()).toBe(true));

    first.drop();
    typeInto(connection.doc, " typed offline");
    const clientId = connection.doc.clientID;
    await waitFor(async () => expect(await rowsOf(artifact)).toEqual([[artifact, clientId, 1]]));
    const second = await socket(1);
    second.receive(syncStep1Frame(Y.encodeStateVector(rebuilt), artifact));
    // The frame the provider held while no socket was open goes out as the socket first answers.
    expect(syncPayloads(second, artifact, 2)).toHaveLength(1);
    second.receive(authenticatedFrame("read-write", artifact));
    second.receive(syncStep2Frame(Y.encodeStateAsUpdate(rebuilt), artifact));
    // The typed frame parks behind the replaced history and is answered not applied, and the
    // SyncStep2 that hands that history back is refused.
    second.receive(syncStatusFrame(false, artifact));
    second.receive(syncStatusFrame(false, artifact));
    second.dispatchEvent(
      new CloseEvent("close", {
        code: DOCUMENT_TOO_LARGE_CLOSE_CODE,
        reason: DOCUMENT_REBUILT_CLOSE_REASON,
      })
    );
    await waitFor(() => expect(refusals).toEqual([DOCUMENT_REBUILT_CLOSE_REASON]));
    expect(await rowsOf(artifact)).toEqual([[artifact, clientId, 1]]);
  });

  await withProvider(artifact, async ({ pending, socket, synced }) => {
    serveInYgoOrder(await socket(0), rebuilt, "read-write", artifact);
    await waitFor(() => expect(synced()).toBe(true));
    expect(await rowsOf(artifact)).toEqual([]);
    const reported = pending.at(-1);
    if (reported?.rebuiltAt === undefined) {
      throw new Error(
        "the document read afresh names no edit it dropped as typed before the rebuild"
      );
    }
    expect(reported.count).toBe(0);
    expect(connectionLabel("connected", pendingNotice("connected", reported))).toBe(
      `connected · Edits saved in this browser at ${new Date(reported.rebuiltAt).toLocaleString()} could not be applied: the document was rebuilt since.`
    );
  });
});

test("a real provider closed as busy reconnects, keeps the edit, and says when it is taken", async () => {
  const artifact = artifactId();
  await withProvider(artifact, async (harness) => {
    const socket = await admitWritable(harness, artifact, 0);
    harness.connection.doc.getText("t").insert(0, "waits for room");
    const clientId = harness.connection.doc.clientID;
    await waitFor(async () => expect(await rowsOf(artifact)).toEqual([[artifact, clientId, 1]]));
    socket.receive(syncStatusFrame(false, artifact));
    socket.dispatchEvent(
      new CloseEvent("close", { code: DOCUMENT_BUSY_CLOSE_CODE, reason: "DOC_BUSY" })
    );

    expect(harness.busy).toEqual([true]);
    await waitFor(() => expect(OpenSocket.opened).toHaveLength(2));
    expect(await rowsOf(artifact)).toEqual([[artifact, clientId, 1]]);
    const reconnected = await harness.socket(1);
    reconnected.receive(authenticatedFrame("read-write", artifact));
    reconnected.receive(syncStep1Frame(Uint8Array.of(0), artifact));
    reconnected.receive(syncStep2Frame(Y.encodeStateAsUpdate(new Y.Doc()), artifact));
    // The reconnected provider's SyncStep2 carries the edit, and the server applies it.
    reconnected.receive(syncStatusFrame(true, artifact));

    await waitFor(() => expect(harness.busy).toEqual([true, false]));
    await waitFor(async () => expect(await rowsOf(artifact)).toEqual([]));
    expect(harness.refusals).toEqual([]);
  });
});

test("a busy close that arrives after an applied answer keeps the busy notice up", async () => {
  const artifact = artifactId();
  await withProvider(artifact, async (harness) => {
    const socket = await admitWritable(harness, artifact, 0);
    const text = harness.connection.doc.getText("t");
    text.insert(0, "waits for room");
    const clientId = harness.connection.doc.clientID;
    await waitFor(async () => expect(await rowsOf(artifact)).toEqual([[artifact, clientId, 1]]));
    socket.receive(syncStatusFrame(false, artifact));
    socket.dispatchEvent(
      new CloseEvent("close", { code: DOCUMENT_BUSY_CLOSE_CODE, reason: "DOC_BUSY" })
    );
    expect(harness.busy).toEqual([true]);
    await waitFor(() => expect(OpenSocket.opened).toHaveLength(2));
    const reconnected = await harness.socket(1);
    reconnected.receive(authenticatedFrame("read-write", artifact));
    reconnected.receive(syncStep1Frame(Uint8Array.of(0), artifact));
    reconnected.receive(syncStep2Frame(Y.encodeStateAsUpdate(new Y.Doc()), artifact));
    // The server applies the reconnected provider's SyncStep2, then turns this browser's next edit
    // away for want of room, in the same turn: the busy close lands while the pending sync is
    // still clearing the row the applied answer acknowledged.
    reconnected.receive(syncStatusFrame(true, artifact));
    reconnected.dispatchEvent(
      new CloseEvent("close", { code: DOCUMENT_BUSY_CLOSE_CODE, reason: "DOC_BUSY" })
    );

    // The row's clear is what the applied answer waits on, and its transaction completes before
    // the read that finds it gone, so the answer has been handled by the time this reads it.
    await waitFor(async () => expect(await rowsOf(artifact)).toEqual([]));
    expect(harness.busy).toEqual([true]);
  });
});

/** The stateless frame Dispatch writes first on a socket, giving the page `client` (`DOCUMENT_CLIENT_ID_MESSAGE_PREFIX`). */
function clientIdFrame(client: number, name: string): Uint8Array {
  return frame(
    name,
    [5],
    varBytes(new TextEncoder().encode(`${DOCUMENT_CLIENT_ID_MESSAGE_PREFIX}${client}`))
  );
}

/** The client id the socket presented when it opened (`DOCUMENT_CLIENT_ID_PARAMETER`). */
function presentedClientId(socket: OpenSocket): string | null {
  return new URL(socket.url).searchParams.get(DOCUMENT_CLIENT_ID_PARAMETER);
}

/** Every client an awareness frame the socket wrote names, in order. */
function announcedClients(socket: OpenSocket, name: string): number[] {
  const prefix = frame(name, [1]);
  return socket.sent
    .filter((written) => prefix.every((byte, index) => written[index] === byte))
    .flatMap((written) => {
      const decoder = createDecoder(written);
      readVarString(decoder); // the document name
      readVarUint(decoder); // Awareness
      const update = createDecoder(readVarUint8Array(decoder));
      const clients: number[] = [];
      for (let count = readVarUint(update); count > 0; count -= 1) {
        clients.push(readVarUint(update));
        readVarUint(update); // the clock
        readVarString(update); // the state
      }
      return clients;
    });
}

/** The client of every struct in the Update frames a socket wrote. */
function updateClients(socket: OpenSocket, name: string): number[] {
  return syncPayloads(socket, name, 2).flatMap((update) =>
    Y.decodeUpdate(update).structs.map((struct) => struct.id.client)
  );
}

test("a page takes the client id its socket's first frame gives it, announces and writes under it alone, and presents it on every reconnect", async () => {
  const artifact = artifactId();
  const server = serverWith("shared", 100);
  const given = 2 ** 32 + 4_321;
  await withProvider(artifact, async ({ connection, refusals, socket, synced }) => {
    const first = await socket(0);
    expect(presentedClientId(first)).toBe("");
    // The provider's own announcement at the socket's open carries nothing: the page has no id yet.
    expect(announcedClients(first, artifact)).toEqual([]);
    first.receive(clientIdFrame(given, artifact));
    expect(connection.doc.clientID).toBe(given);
    expect(connection.awareness.clientID).toBe(given);
    serveInYgoOrder(first, server, "read-write", artifact);
    await waitFor(() => expect(synced()).toBe(true));
    typeInto(connection.doc, " typed");
    await waitFor(() => expect(updateClients(first, artifact)).toEqual([given]));
    await waitFor(async () => expect(await rowsOf(artifact)).toEqual([[artifact, given, 1]]));
    expect(new Set(announcedClients(first, artifact))).toEqual(new Set([given]));

    first.drop();
    const second = await socket(1);
    expect(presentedClientId(second)).toBe(String(given));
    second.receive(clientIdFrame(given, artifact));
    serveInYgoOrder(second, server, "read-write", artifact);
    typeInto(connection.doc, " again");
    await waitFor(() => expect(updateClients(second, artifact)).toContain(given));
    expect(connection.doc.clientID).toBe(given);
    expect(new Set(announcedClients(second, artifact))).toEqual(new Set([given]));
    expect(refusals).toEqual([]);
  });
});

test("a page that has written under its client id and is given another is refused as an edit under an id it may not write under", async () => {
  const artifact = artifactId();
  const server = serverWith("shared", 100);
  const given = 2 ** 32 + 7;
  await withProvider(artifact, async ({ connection, refusals, socket, synced }) => {
    const first = await socket(0);
    first.receive(clientIdFrame(given, artifact));
    serveInYgoOrder(first, server, "read-write", artifact);
    await waitFor(() => expect(synced()).toBe(true));
    typeInto(connection.doc, " typed");
    await waitFor(() => expect(updateClients(first, artifact)).toEqual([given]));

    first.drop();
    const second = await socket(1);
    second.receive(clientIdFrame(given + 1, artifact));
    await waitFor(() => expect(refusals).toEqual([DOCUMENT_CLIENT_ID_REFUSED_CLOSE_REASON]));
    expect(connection.doc.clientID).toBe(given);
  });
});

// The room took every edit the page typed under the id its first socket gave it, and a later
// socket gives it another: its person signed in again as someone else, say, whose admission is
// given an id of its own (client_ids_admission.go, keepsClient). The page is refused as one
// holding edits under an id it may not write under, though no saved row goes with the refusal:
// the room holds every edit, so the notice names none as lost.
test("a page refused its client id after the room took every edit under it is told no edit went", async () => {
  const artifact = artifactId();
  const server = serverWith("shared", 100);
  const given = 2 ** 32 + 21;
  await withProvider(artifact, async ({ connection, dropped, refusals, socket, synced }) => {
    const first = await socket(0);
    first.receive(clientIdFrame(given, artifact));
    serveInYgoOrder(first, server, "read-write", artifact);
    await waitFor(() => expect(synced()).toBe(true));
    typeInto(connection.doc, " typed");
    await waitFor(async () => expect(await rowsOf(artifact)).toEqual([[artifact, given, 1]]));
    answerUpdates(first, server, artifact);
    await waitFor(async () => expect(await rowsOf(artifact)).toEqual([]));

    first.drop();
    const second = await socket(1);
    second.receive(clientIdFrame(given + 1, artifact));
    await waitFor(() => expect(refusals).toEqual([DOCUMENT_CLIENT_ID_REFUSED_CLOSE_REASON]));
    expect(dropped).toEqual([0]);
    expect(refusedEditNotice(DOCUMENT_CLIENT_ID_REFUSED_CLOSE_REASON, dropped[0])).toBe(
      "Dispatch has reloaded the document: this page held edits under an editing identity it may not write under. Every edit of yours that reached Dispatch is kept, and those saved in this browser are sent again."
    );
  });
});

// A page that has typed nothing under the id its first socket gave it takes the one a later
// socket gives it instead (`takeClientId`): its person signed in again as someone else, say. Its
// saved rows stay keyed by the first id (`PendingEdits.keyed`), so what it types after is saved
// under that key and written under the new id, and is still this page's own: a read-only
// admission, which judges the rows of other tabs, leaves it saved until the room takes it.
test("a page given another client id before it typed keeps what it types after as its own saved rows", async () => {
  const artifact = artifactId();
  const server = serverWith("shared", 100);
  const firstGiven = 2 ** 32 + 31;
  const laterGiven = 2 ** 32 + 32;
  // Each admission's restore or prune of the saved rows, which the transport starts.
  const running: Promise<void>[] = [];
  const realStart = startPendingSync;
  const started = spyOn(pendingSyncModule, "startPendingSync").mockImplementation((options) => {
    const sync = realStart(options);
    return {
      ...sync,
      synced() {
        const admitting = sync.synced();
        running.push(admitting);
        return admitting;
      },
    };
  });
  try {
    await withProvider(artifact, async ({ connection, pending, socket, synced }) => {
      const first = await socket(0);
      first.receive(clientIdFrame(firstGiven, artifact));
      serveInYgoOrder(first, server, "read-write", artifact);
      await waitFor(() => expect(synced()).toBe(true));

      first.drop();
      const second = await socket(1);
      second.receive(clientIdFrame(laterGiven, artifact));
      expect(connection.doc.clientID).toBe(laterGiven);
      serveInYgoOrder(second, server, "read-write", artifact);
      typeInto(connection.doc, " typed");
      await waitFor(() => expect(updateClients(second, artifact)).toEqual([laterGiven]));
      await waitFor(async () =>
        expect(await rowsOf(artifact)).toEqual([[artifact, firstGiven, 1]])
      );

      // The issue closes before the room takes the edit, and the socket reconnects read-only.
      second.drop();
      const third = await socket(2);
      third.receive(clientIdFrame(laterGiven, artifact));
      serveInYgoOrder(third, server, "readonly", artifact);
      await Promise.all(running);
      expect(await rowsOf(artifact)).toEqual([[artifact, firstGiven, 1]]);
      expect(pending.at(-1)).toMatchObject({ count: 1, readOnly: true });
    });
  } finally {
    started.mockRestore();
  }
});

// A read-only admission stores no client id for the page: Dispatch sends it one drawn for it alone,
// which no row names (client_ids_admission.go, admitClients). The page takes it, having written
// nothing, and presents it on its next socket; once that socket is admitted writable, Dispatch
// finds no row for it and gives one afresh, which the page takes too (`takeClientId`), since a
// read-only page writes nothing. What it types then goes out under the fresh id, is kept as a
// saved row until the room acknowledges it, and the room holds it: no refusal, and nothing lost.
test("a page admitted read-only and then writable takes the fresh client id its writable socket gives it and writes under it", async () => {
  const artifact = artifactId();
  const server = serverWith("shared", 100);
  const drawn = 2 ** 32 + 51;
  const fresh = 2 ** 32 + 52;
  await withProvider(artifact, async ({ connection, refusals, socket, synced }) => {
    const first = await socket(0);
    first.receive(clientIdFrame(drawn, artifact));
    expect(connection.doc.clientID).toBe(drawn);
    serveInYgoOrder(first, server, "readonly", artifact);
    await waitFor(() => expect(synced()).toBe(true));
    expect(updateClients(first, artifact)).toEqual([]);

    first.drop();
    const second = await socket(1);
    expect(presentedClientId(second)).toBe(String(drawn));
    second.receive(clientIdFrame(fresh, artifact));
    expect(connection.doc.clientID).toBe(fresh);
    expect(connection.awareness.clientID).toBe(fresh);
    serveInYgoOrder(second, server, "read-write", artifact);
    typeInto(connection.doc, " typed");
    await waitFor(() => expect(updateClients(second, artifact)).toEqual([fresh]));
    await waitFor(async () => expect(await rowsOf(artifact)).toEqual([[artifact, drawn, 1]]));
    answerUpdates(second, server, artifact);
    expect(server.getXmlFragment("prosemirror").toString()).toContain(" typed");
    await waitFor(async () => expect(await rowsOf(artifact)).toEqual([]));
    expect(refusals).toEqual([]);
  });
});

test("a server that gives no client id leaves the page the id its document drew, and its presence goes out once that is known", async () => {
  const artifact = artifactId();
  const server = serverWith("shared", 100);
  await withProvider(artifact, async ({ connection, socket, synced }) => {
    const drawn = connection.doc.clientID;
    const first = await socket(0);
    expect(connection.awareness.getLocalState()).toBeNull();
    serveInYgoOrder(first, server, "read-write", artifact);
    await waitFor(() => expect(synced()).toBe(true));
    expect(connection.doc.clientID).toBe(drawn);
    expect(connection.awareness.getLocalState()).toEqual({});
    typeInto(connection.doc, " typed");
    await waitFor(async () => expect(await rowsOf(artifact)).toEqual([[artifact, drawn, 1]]));
  });
});

// What a tab of the build before client ids sends once the reader reloads it: the page of this
// build is given an id of its own, and replays the row the earlier page saved under the id that
// page drew for itself, which is below 2^32, so the server must take an edit under an id it never
// gave out (client_ids.go, claimClients).
test("a reloaded page replays a row an earlier page saved under the client id that page drew", async () => {
  const artifact = artifactId();
  const server = serverWith("shared", 100);
  const [saved] = await savedChanges(artifact, server, (browser) => typeInto(browser, " saved"));
  if (saved === undefined) {
    throw new Error("one change saves one row");
  }
  const earlier = saved.key[1];
  const given = 2 ** 32 + 99;
  await withProvider(artifact, async ({ connection, socket, synced }) => {
    const first = await socket(0);
    first.receive(clientIdFrame(given, artifact));
    serveInYgoOrder(first, server, "read-write", artifact);
    await waitFor(() => expect(synced()).toBe(true));
    await waitFor(() => expect(paragraphText(connection.doc).toString()).toBe("shared saved"));
    await waitFor(() => expect(updateClients(first, artifact)).toEqual([earlier]));
    expect(earlier).toBeLessThan(2 ** 32);
    expect(connection.doc.clientID).toBe(given);
  });
});

// A reload while the network is down: the page's socket is never answered, so the page never
// learns the client id it would write under, and it still names the edits an earlier page saved
// in this browser ("1 edit saved in this browser, not sent yet.", `pendingNotice`).
test("a page whose socket has not answered names the edits an earlier page saved in this browser", async () => {
  const artifact = artifactId();
  await savedChanges(artifact, serverWith("shared", 100), (browser) => typeInto(browser, " saved"));
  await withProvider(artifact, async ({ pending, socket }) => {
    // The provider has written its first frames, and the test, as the server, answers none.
    await socket(0);
    await waitFor(() =>
      expect(pending).toEqual([
        { count: 1, readOnly: false, rebuiltAt: undefined, stored: true, unsent: false },
      ])
    );
  });
});

// A refusal of a replayed row as under another person's client id: what a reloaded tab of the
// build before client ids meets when someone claimed its id first (client_ids.go, claimClients).
// Every saved row under the refused id goes at once, and the host is told how many; a row another
// earlier tab saved under an id of its own stays, to be replayed by the rebuilt document.
test("a refusal of a row under another person's client id drops every row under that id at once and keeps the rest", async () => {
  const artifact = artifactId();
  const server = serverWith("shared", 100);
  const claimed = await savedChanges(
    artifact,
    server,
    (browser) => typeInto(browser, " first"),
    (browser) => typeInto(browser, " second")
  );
  const [kept] = await savedChanges(artifact, server, (browser) => typeInto(browser, " kept"));
  const refusedClient = claimed[0]?.key[1];
  const keptClient = kept?.key[1];
  if (refusedClient === undefined || keptClient === undefined) {
    throw new Error("each change saves one row");
  }
  const given = 2 ** 32 + 5;
  await withProvider(artifact, async ({ dropped, refusals, socket, synced }) => {
    const first = await socket(0);
    first.receive(clientIdFrame(given, artifact));
    serveInYgoOrder(first, server, "read-write", artifact);
    await waitFor(() => expect(synced()).toBe(true));
    await waitFor(() => expect(syncPayloads(first, artifact, 2)).toHaveLength(3));
    await waitFor(async () => expect(await rowsOf(artifact)).toHaveLength(3));
    // The replays go out in the store's order; the server takes each before the first under the
    // claimed id and refuses that one.
    const writers = syncPayloads(first, artifact, 2).map(
      (update) => Y.decodeUpdate(update).structs[0]?.id.client
    );
    const refusedAt = writers.indexOf(refusedClient);
    for (let i = 0; i < refusedAt; i += 1) {
      first.receive(syncStatusFrame(true, artifact));
    }
    first.receive(syncStatusFrame(false, artifact));
    first.dispatchEvent(
      new CloseEvent("close", {
        code: DOCUMENT_TOO_LARGE_CLOSE_CODE,
        reason: DOCUMENT_CLIENT_ID_REFUSED_CLOSE_REASON,
      })
    );
    await waitFor(() => expect(refusals).toEqual([DOCUMENT_CLIENT_ID_REFUSED_CLOSE_REASON]));
    expect(dropped).toEqual([2]);
    const left = async () =>
      (await (await storeFor(artifact, -1)).restoreRows()).map(
        (row) => Y.decodeUpdate(row.update).structs[0]?.id.client
      );
    await waitFor(async () =>
      expect(await left()).toEqual(writers.indexOf(keptClient) < refusedAt ? [] : [keptClient])
    );
    expect(refusedEditNotice(DOCUMENT_CLIENT_ID_REFUSED_CLOSE_REASON, 2)).toBe(
      "2 of your edits were not saved: they were made under an editing identity this page may not write under. Dispatch has reloaded the document."
    );
  });
});

/** The step (0 SyncStep1, 1 SyncStep2, 2 Update) of each Sync frame a socket wrote, in order. */
function syncSteps(socket: OpenSocket, name: string): number[] {
  const prefix = frame(name, [0]);
  return socket.sent
    .filter((written) => prefix.every((byte, index) => written[index] === byte))
    .map((written) => {
      const decoder = createDecoder(written);
      readVarString(decoder); // the document name
      readVarUint(decoder); // Sync
      return readVarUint(decoder);
    });
}

/** A Sync Update frame the room sends: what it relays of another browser's edit. */
function relayedFrame(update: Uint8Array, name: string): Uint8Array {
  return frame(name, [0, 2], varBytes(update));
}

// The room relayed another person's edit to this page and then lost it before its store, and the
// page's person typed right after it while the socket was down. The reconnect's SyncStep2 hands
// back the lost edit beside the person's own, and the room refuses it whole as an edit under an id
// this person may not write under (client_ids_judgement.go, judgeClients). A SyncStep2 names no
// saved row, so none goes: the row is kept, and the notice says the document was reloaded and the
// person's saved edits are sent again. The document read afresh from the room (ProofDocument's
// `onEditRefused`) does send it: the row parks there on the other person's lost edit, which no
// rebuild retired, so the restore records it again and sends it as an Update of its own. The room
// holds it back and answers it not applied, and the row stays saved until the room has landed it
// beside the resent edit and the SyncStep2 that answers the room's resync is answered applied.
test("a SyncStep2 refused for another person's lost edit keeps the page's own row, and the document read afresh sends it until it lands", async () => {
  const artifact = artifactId();
  const server = serverWith("shared", 100);
  const given = 2 ** 32 + 41;
  const alice = new Y.Doc();
  alice.clientID = 2 ** 32 + 42;
  Y.applyUpdate(alice, Y.encodeStateAsUpdate(server));
  typeInto(alice, " L");
  const lostEdit = Y.encodeStateAsUpdate(alice, Y.encodeStateVector(server));
  await withProvider(artifact, async ({ connection, dropped, refusals, socket, synced }) => {
    const first = await socket(0);
    first.receive(clientIdFrame(given, artifact));
    serveInYgoOrder(first, server, "read-write", artifact);
    await waitFor(() => expect(synced()).toBe(true));
    // The room relays alice's edit, and then loses it before its store: its socket drops.
    first.receive(relayedFrame(lostEdit, artifact));
    await waitFor(() => expect(paragraphText(connection.doc).toString()).toBe("shared L"));
    first.drop();
    typeInto(connection.doc, " D");
    await waitFor(async () => expect(await rowsOf(artifact)).toEqual([[artifact, given, 1]]));

    // The reconnect: the room, which lacks alice's edit, asks for what the page holds.
    const second = await socket(1);
    second.receive(clientIdFrame(given, artifact));
    second.receive(syncStep1Frame(Y.encodeStateVector(server), artifact));
    second.receive(syncStep2Frame(Y.encodeStateAsUpdate(server), artifact));
    second.receive(awarenessFrame(artifact));
    const handed = syncPayloads(second, artifact, 1).at(-1);
    if (handed === undefined) {
      throw new Error("the provider answers the room's SyncStep1 with a SyncStep2");
    }
    expect(Y.decodeUpdate(handed).structs.map((struct) => struct.id.client)).toContain(
      alice.clientID
    );
    // The room answers the frames in order: the queued Update of the typing parks, not applied,
    // and the SyncStep2 is refused and the socket closed.
    for (const step of syncSteps(second, artifact).filter((written) => written !== 0)) {
      second.receive(syncStatusFrame(false, artifact));
      if (step === 1) {
        break;
      }
    }
    second.dispatchEvent(
      new CloseEvent("close", {
        code: DOCUMENT_TOO_LARGE_CLOSE_CODE,
        reason: DOCUMENT_CLIENT_ID_REFUSED_CLOSE_REASON,
      })
    );
    await waitFor(() => expect(refusals).toEqual([DOCUMENT_CLIENT_ID_REFUSED_CLOSE_REASON]));
    expect(dropped).toEqual([0]);
    expect(await rowsOf(artifact)).toEqual([[artifact, given, 1]]);
    expect(refusedEditNotice(DOCUMENT_CLIENT_ID_REFUSED_CLOSE_REASON, dropped[0])).toBe(
      "Dispatch has reloaded the document: this page held edits under an editing identity it may not write under. Every edit of yours that reached Dispatch is kept, and those saved in this browser are sent again."
    );
  });

  // The document read afresh from the room, under a fresh id.
  const rebuiltGiven = 2 ** 32 + 43;
  await withProvider(artifact, async ({ connection, pending, socket, synced }) => {
    const first = await socket(0);
    first.receive(clientIdFrame(rebuiltGiven, artifact));
    serveInYgoOrder(first, server, "read-write", artifact);
    await waitFor(() => expect(synced()).toBe(true));
    await waitFor(() => expect(updateClients(first, artifact)).toEqual([given]));
    await waitFor(async () =>
      expect(await rowsOf(artifact)).toEqual([[artifact, rebuiltGiven, 1]])
    );
    await waitFor(() => expect(pending.at(-1)).toMatchObject({ count: 1, rebuiltAt: undefined }));
    expect(paragraphText(connection.doc).toString()).toBe("shared");

    // The room holds the replayed row back, behind alice's lost edit.
    first.receive(syncStatusFrame(false, artifact));
    expect(await rowsOf(artifact)).toEqual([[artifact, rebuiltGiven, 1]]);

    // Alice's browser sends her edit again: the room lands it and the row, relays both, and once
    // it has stored them resyncs this page, whose SyncStep2 it answers applied.
    const [replayed] = syncPayloads(first, artifact, 2);
    if (replayed === undefined) {
      throw new Error("the document read afresh sends the saved row as an Update");
    }
    const held = Y.encodeStateVector(connection.doc);
    Y.applyUpdate(server, lostEdit);
    Y.applyUpdate(server, replayed);
    first.receive(relayedFrame(Y.encodeStateAsUpdate(server, held), artifact));
    await waitFor(() => expect(paragraphText(connection.doc).toString()).toBe("shared L D"));
    expect(await rowsOf(artifact)).toEqual([[artifact, rebuiltGiven, 1]]);
    first.receive(syncStep1Frame(Y.encodeStateVector(server), artifact));
    await waitFor(() => expect(syncPayloads(first, artifact, 1).length).toBeGreaterThan(1));
    first.receive(syncStatusFrame(true, artifact));
    await waitFor(async () => expect(await rowsOf(artifact)).toEqual([]));
    await waitFor(() => expect(pending.at(-1)?.count).toBe(0));
  });
});
