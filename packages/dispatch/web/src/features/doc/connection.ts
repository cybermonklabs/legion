import type { HocuspocusProvider } from "@hocuspocus/provider";
import { DOCUMENT_SCHEMA_CLOSE_CODE } from "@legion/contracts";
import type { Awareness } from "y-protocols/awareness";
import type { Doc } from "yjs";

import { reconnectDelayMs } from "../../api/live-socket";
import { importWhenOnline } from "../shell/DeploymentResilience";
import type { PendingState, PendingSync } from "./pending-sync";

const presenceColors = ["#0284c7", "#7c3aed", "#c2410c", "#047857", "#be123c", "#4338ca"] as const;

export type ConnectionState = "connecting" | "connected" | "offline" | "failed";
function savedEdits(count: number): string {
  return count === 1 ? "1 edit" : `${count} edits`;
}

export function pendingNotice(
  connection: ConnectionState,
  pending: PendingState | undefined
): string | undefined {
  if (pending === undefined) {
    return undefined;
  }
  if (pending.rebuiltAt !== undefined) {
    return `Edits saved in this browser at ${new Date(pending.rebuiltAt).toLocaleString()} could not be applied: the document was rebuilt since.`;
  }
  if (pending.readOnly && pending.count > 0) {
    return `${savedEdits(pending.count)} saved in this browser can't be sent: this document is read-only.`;
  }
  if (connection === "offline" || connection === "connecting") {
    if (!pending.stored) {
      return "Edits typed now will not survive a reload.";
    }
    if (pending.count > 0) {
      return `${savedEdits(pending.count)} saved in this browser, not sent yet.`;
    }
  }
  return undefined;
}

/** The connection's name, followed by the pending-edit notice when there is one. */
export function connectionLabel(
  connection: ConnectionState,
  notice: string | undefined = undefined
): string {
  let label: string;
  switch (connection) {
    case "connecting":
      label = "Connecting to the document…";
      break;
    case "failed":
      label = "The document could not load";
      break;
    default:
      label = connection;
  }
  return notice === undefined ? label : `${label} · ${notice}`;
}

export interface DocumentConnection {
  readonly awareness: Awareness;
  readonly doc: Doc;
  destroy(): void;
  /**
   * Has the server decide a read-only admission again: the server decides a socket's admission
   * once, as it connects, so this closes a socket admitted read-only, the provider reconnects as
   * it does after any close the server makes, and the new socket's admission reaches
   * `onAdmission` and the pending sync. A socket admitted writable keeps its admission. A socket
   * the server has not answered yet may be one the provider opened after the server's own close,
   * which the server admits as it decides now, so its answer decides: a read-only one closes it,
   * unreported. Closing that socket regardless would cost a second reconnect, and its draw.
   */
  readmit(): void;
}

export interface ConnectionCallbacks {
  /** The editor's tree: the fragment of the room that holds this document (`fragmentOf`). The
   *  socket names the document, and the server decides the room. */
  fragment: string;
  /** V, the schema version of the editor this connection serves: its manifest's `version`. */
  schemaVersion: number;
  /** R, the highest `since` among the elements that editor may create (`schemaCreatable`). */
  schemaCreatable: number;
  onAdmission(readOnly: boolean): void;
  /**
   * Fires when the server refuses the socket because the stored document is outside the Proof
   * schema. The connection has stopped reconnecting; a fresh read decides what comes next.
   */
  onOutsideSchema(): void;
  onStatus(state: ConnectionState): void;
  /** Reports browser-held edits as IndexedDB changes or this admission's restore completes. */
  onPending(state: PendingState): void;
  /** Fires once, when the server's first sync completes after admission. */
  onSynced(): void;
  /**
   * Splits the server's first sync, a Yjs V1 update, into parts that applied in order give the
   * same document (the page's is `splitSyncInWorker` of `issue-page/sync-parts.ts`). Where it is
   * given, the connection applies that sync one part to a task ("The first sync in parts" in
   * `loadDocumentTransport`), and a server's close with `DOCUMENT_SCHEMA_CLOSE_CODE` stops it only
   * at the second in a row on a socket that had no message. Without it, the connection applies the
   * first sync whole, as the provider does, and stops at any such close.
   */
  firstSyncInParts?: (update: Uint8Array) => Promise<readonly Uint8Array[]>;
}

export type ConnectDocument = (
  artifactId: string,
  callbacks: ConnectionCallbacks
) => DocumentConnection;

export function colorForLogin(login: string): string {
  let hash = 0;
  for (const character of login) {
    hash = (hash * 31 + character.charCodeAt(0)) | 0;
  }
  return presenceColors[Math.abs(hash) % presenceColors.length] ?? presenceColors[0];
}

export function isSchemaReadOnly(scope: string | undefined): boolean {
  return scope === "readonly";
}

export function wsUrl(artifactId: string): string {
  const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
  return `${protocol}//${window.location.host}/ws/doc/${encodeURIComponent(artifactId)}`;
}

/** The browser's `scheduler`, by the one call the first sync's parts make of it. */
interface TaskScheduler {
  postTask(callback: () => void): Promise<unknown>;
}

/**
 * Resolves in a task of its own, so each part of the first sync is applied in one: a task of
 * `scheduler.postTask` where the browser has it (Chromium's and Firefox's), which runs a chain of
 * tasks with no wait between them, else of `setTimeout(0)`, which the browser waits 4 ms for from
 * the fifth in a chain. `askForTask` (`issue-page/drawn-in-slices.ts`) asks for its tasks the
 * same way; it is not imported here, since its module brings React and the page's editing hooks.
 */
function nextTask(): Promise<void> {
  const task = Promise.withResolvers<void>();
  const scheduler: Partial<TaskScheduler> = Reflect.get(globalThis, "scheduler") ?? {};
  if (scheduler.postTask !== undefined) {
    void scheduler.postTask(() => task.resolve());
  } else {
    setTimeout(task.resolve, 0);
  }
  return task.promise;
}

// Hocuspocus and Yjs load with the first document that connects, not with the issue route: the
// same lazy boundary `createEditor` puts around the editor itself. The connect they resolve to is
// synchronous, so a host has one place to react to the connection.
export async function loadDocumentTransport(): Promise<ConnectDocument> {
  const [
    { HocuspocusProvider, HocuspocusProviderWebsocket },
    { Doc },
    { openPendingEdits },
    { startPendingSync },
    { MessageType },
    { createDecoder, readVarString, readVarUint, readVarUint8Array },
    { messageYjsSyncStep2 },
    { applyUpdate },
  ] = await importWhenOnline(() =>
    Promise.all([
      import("@hocuspocus/provider"),
      import("yjs"),
      import("./pending-edits"),
      import("./pending-sync"),
      import("@hocuspocus/provider"),
      import("lib0/decoding"),
      import("y-protocols/sync"),
      import("yjs"),
    ])
  );
  return (artifactId, callbacks) => {
    const doc = new Doc();
    let destroyed = false;
    // Whether any admission has been handed to the pending sync: the first one's restore ends in
    // the host's `onSynced`.
    let announced = false;
    let provider: HocuspocusProvider;
    // `connect: false` below means this class cannot instantiate a socket until `sync` is assigned
    // after the provider exists; the explicit `websocket.connect()` is deliberately last.
    let sync: PendingSync | undefined;
    // The socket the server last answered (its Authenticated frame), and whether it admitted that
    // socket read-only.
    let admitted: { readOnly: boolean; socket: WebSocket } | undefined;
    // The socket a readmit found unanswered: a read-only answer closes it (`readmit`). It may name
    // a socket that closed before it answered; no later socket can equal that one, so each later
    // socket is judged by its own admission, as if no readmit had found it.
    let readmitOnAnswer: WebSocket | undefined;
    // The first sync in parts, where the host gives `firstSyncInParts`. From `connect` until the
    // first sync's last part, the document holds no `update` listener, neither the provider's
    // (`boundDocumentUpdateHandler`) nor the pending sync's, so Yjs writes no update message for
    // a part and neither takes one for an edit. The first SyncStep2 any socket receives is taken
    // in that socket's own `message` listener, which runs before the provider's, and stopped
    // there; every frame a socket receives after it is held, in order, and a socket's held frames
    // are dropped as it closes. The parts are applied one to a task, with the provider as their
    // origin, as the provider applies the server's updates. After the last part both listeners go
    // back, and the socket hands the provider that SyncStep2 with an empty update, which sets
    // `synced` and so runs `notifyAdmitted` as the base does, then each held frame, in order. Every
    // later SyncStep2, a reconnect's, reaches the provider untouched, in one part.
    const split = callbacks.firstSyncInParts;
    let firstSync: "waiting" | "applying" | "applied" = split === undefined ? "applied" : "waiting";
    let held: { data: ArrayBufferLike; socket: WebSocket }[] = [];
    // Set while the connection hands a socket's listeners a frame itself, which its own passes on.
    let delivering = false;
    // How many sockets in a row the server closed with `DOCUMENT_SCHEMA_CLOSE_CODE` before any
    // message (`onClose` below).
    let refusedInARow = 0;
    const deliver = (socket: WebSocket, data: ArrayBufferLike) => {
      delivering = true;
      try {
        socket.dispatchEvent(new MessageEvent("message", { data }));
      } finally {
        delivering = false;
      }
    };
    // A frame's update where the frame is a SyncStep2, with the same frame holding an empty
    // update in its place: no client's structs and no delete (the two zero counts of
    // `Y.encodeStateAsUpdate(new Y.Doc())`), after their length, 2.
    const syncStep2Of = (frame: Uint8Array) => {
      try {
        const decoder = createDecoder(frame);
        readVarString(decoder); // the document name
        if (readVarUint(decoder) !== MessageType.Sync) return undefined;
        if (readVarUint(decoder) !== messageYjsSyncStep2) return undefined;
        const head = decoder.pos;
        const update = readVarUint8Array(decoder);
        return { empty: Uint8Array.of(...frame.subarray(0, head), 2, 0, 0).buffer, update };
      } catch {
        // The provider reads a frame this cannot, and reports it as it does at the base.
        return undefined;
      }
    };
    const applyInParts = async (
      splitUpdate: (update: Uint8Array) => Promise<readonly Uint8Array[]>,
      update: Uint8Array
    ) => {
      let parts: readonly Uint8Array[];
      try {
        parts = await splitUpdate(update);
      } catch (error) {
        console.error("Could not split the document's first sync, so it is applied whole", error);
        parts = [update];
      }
      for (const part of parts) {
        await nextTask();
        if (destroyed) return;
        try {
          applyUpdate(doc, part, provider);
        } catch (error) {
          // As the provider's own apply of a SyncStep2 (y-protocols' `readSyncStep2`) reports it.
          console.error("Caught error while handling a Yjs update", error);
        }
      }
      if (destroyed) return;
      doc.on("update", provider.boundDocumentUpdateHandler);
      sync?.attachUpdates();
      firstSync = "applied";
      for (const frame of held.splice(0)) deliver(frame.socket, frame.data);
    };
    const watchSocket = (
      socket: WebSocket,
      splitUpdate: (update: Uint8Array) => Promise<readonly Uint8Array[]>
    ) => {
      let spoke = false;
      socket.addEventListener("message", (event) => {
        spoke = true;
        if (delivering || firstSync === "applied" || !(event.data instanceof ArrayBuffer)) return;
        if (firstSync === "waiting") {
          const step2 = syncStep2Of(new Uint8Array(event.data));
          if (step2 === undefined) return;
          firstSync = "applying";
          held.push({ data: step2.empty, socket });
          void applyInParts(splitUpdate, step2.update);
        } else {
          held.push({ data: event.data, socket });
        }
        event.stopImmediatePropagation();
        // The provider's socket handler settles the socket's connection attempt on any message,
        // before it reads it, so a close after this one reconnects as it does at the base.
        if (websocket.webSocket === socket) websocket.resolveConnectionAttempt();
      });
      socket.addEventListener("close", (event) => {
        refusedInARow = event.code === DOCUMENT_SCHEMA_CLOSE_CODE && !spoke ? refusedInARow + 1 : 0;
        held = held.filter((frame) => frame.socket !== socket);
      });
    };
    const WebSocketPolyfill = class extends globalThis.WebSocket {
      constructor(url: string, protocols?: string | string[]) {
        super(url, protocols);
        this.addEventListener("open", () => {
          // The provider waits `initialDelay` before the first try of every `connect()`, and
          // calls `connect()` again whenever a socket the server had answered closes: the
          // server's close (a restart or roll closes every tab's socket at the same moment) and
          // the provider's own silent-socket check alike. So the page's first connect waits
          // nothing, and once a socket has opened, the reconnect after it waits a fresh draw of
          // the spread. The provider refuses a fractional delay. Tries that fail inside one
          // `connect()` keep the provider's own jittered backoff.
          websocket.setConfiguration({ initialDelay: Math.floor(reconnectDelayMs(0)) });
          sync?.socketOpened();
        });
        // The provider adds its own listeners once this constructor returns, so these run first.
        if (split !== undefined) {
          watchSocket(this, split);
        }
      }

      override send(data: Parameters<WebSocket["send"]>[0]): void {
        super.send(data);
        if (data instanceof Uint8Array) {
          sync?.frameWritten(data);
        }
      }
    };
    // The server decides admission from these when it accepts the socket. A server that predates
    // `schema_creatable` ignores that parameter.
    const websocket = new HocuspocusProviderWebsocket({
      WebSocketPolyfill,
      connect: false,
      parameters: {
        schema_creatable: callbacks.schemaCreatable,
        schema_version: callbacks.schemaVersion,
      },
      url: wsUrl(artifactId),
    });
    // Every admission's restore waits for its own socket's authentication and SyncStep2 alike,
    // whichever comes last; Dispatch's server queues its SyncStep2 before it reads the socket's
    // token, so the authentication does. A writable admission restores the rows other tabs stored,
    // judging each against this document; before the reconnect's sync applies, the document lacks
    // what the server took meanwhile, so a row that builds on it would look as if it were typed
    // against a rebuilt history and be dropped. The provider reports each once per socket: `synced`
    // only as `isSynced` changes, which a close resets, and `authenticated` once per Authenticated
    // frame, which the server sends once in answer to the one token each socket sends. So this
    // hands the pending sync one admission per socket, and `synced()` takes a repeated one as a
    // no-op all the same.
    const notifyAdmitted = () => {
      if (!provider.isSynced || !provider.isAuthenticated) {
        return;
      }
      const restoring = sync?.synced().catch((error: unknown) => {
        console.error("Could not restore this document's pending browser edits", error);
      });
      if (announced) {
        return;
      }
      announced = true;
      void restoring?.then(() => {
        if (!destroyed) {
          callbacks.onSynced();
        }
      });
    };
    provider = new HocuspocusProvider({
      document: doc,
      name: artifactId,
      onAuthenticated: () => {
        const readOnly = isSchemaReadOnly(provider.authorizedScope);
        const socket = websocket.webSocket;
        if (socket !== null && socket === readmitOnAnswer) {
          readmitOnAnswer = undefined;
          if (readOnly) {
            // A read-only answer on a socket the readmit found unanswered: the provider
            // reconnects, and the next socket's admission is the one the host is told of.
            socket.close();
            return;
          }
        }
        if (socket !== null) {
          admitted = { readOnly, socket };
        }
        // Whether the admission is read-only decides from here on whether the editor takes typing
        // and whether the pending sync stores a local update as a person's edit.
        sync?.authenticated(readOnly);
        callbacks.onAdmission(readOnly);
        notifyAdmitted();
      },
      onClose: ({ event }) => {
        // Hocuspocus answers every other close by reconnecting. This one is the server's decision
        // about the stored document (`DOCUMENT_SCHEMA_CLOSE_CODE`), so the provider stops here and
        // the host reads it again.
        // With the first sync in parts, the connection stops only at the second such close in a
        // row on a socket that had no message. The server sends the code before any message on a
        // socket it refuses at its upgrade: once where a schema switch overtook the admission
        // (`refuseSwitchedAdmission`), which a second try passes, and on every try where the room
        // is outside the schema (`refuseOutsideSchema`). After a message it is a live schema
        // switch, after which the provider connects again on the same document, as after a roll.
        if (split !== undefined && refusedInARow < 2) {
          return;
        }
        if (event.code === DOCUMENT_SCHEMA_CLOSE_CODE) {
          provider.disconnect();
          callbacks.onOutsideSchema();
        }
      },
      onMessage: ({ event }) => {
        if (event.data instanceof ArrayBuffer) {
          void sync?.frameReceived(new Uint8Array(event.data));
        }
      },
      onStatus: ({ status }) => callbacks.onStatus(status === "disconnected" ? "offline" : status),
      // The provider sets `isSynced` before it reports a change: `true` once the socket's
      // SyncStep2 has been applied, `false` as the socket closes, before `onClose` above runs.
      onSynced: notifyAdmitted,
      // Each document owns its socket. A transport's destroy closes this provider and then the
      // websocket itself, so its connection checker and reconnect attempt cannot outlive it.
      preserveConnection: false,
      // The server decides admission again from the token when the provider authenticates, and
      // that answer replaces the first. A server that predates `schema_creatable` compares the
      // whole token with its own schema version, so the token stays the version alone: any other
      // token would make every socket of that server read-only.
      token: String(callbacks.schemaVersion),
      websocketProvider: websocket,
    });
    const awareness = provider.awareness;
    if (awareness === null) {
      provider.destroy();
      websocket.destroy();
      doc.destroy();
      throw new Error("Dispatch document provider did not create awareness.");
    }
    sync = startPendingSync({
      doc,
      fragment: callbacks.fragment,
      onChange: callbacks.onPending,
      remoteOrigin: provider,
      store: openPendingEdits(artifactId, doc.clientID),
    });
    if (split !== undefined) {
      doc.off("update", provider.boundDocumentUpdateHandler);
      sync.detachUpdates();
    }
    void websocket.connect();
    return {
      awareness,
      destroy() {
        if (destroyed) {
          return;
        }
        destroyed = true;
        sync?.destroy();
        provider.destroy();
        websocket.destroy();
        doc.destroy();
      },
      doc,
      readmit() {
        const socket = websocket.webSocket;
        // With no socket, the provider's next one connects after this call.
        if (destroyed || socket === null) {
          return;
        }
        if (admitted === undefined || admitted.socket !== socket) {
          readmitOnAnswer = socket;
          return;
        }
        if (admitted.readOnly) {
          socket.close();
        }
      },
    };
  };
}
