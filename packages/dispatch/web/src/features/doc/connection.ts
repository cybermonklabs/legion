import type { HocuspocusProvider } from "@hocuspocus/provider";
import {
  DOCUMENT_BUSY_CLOSE_CODE,
  DOCUMENT_CLIENT_ID_PARAMETER,
  DOCUMENT_CLIENT_ID_REFUSED_CLOSE_REASON,
  DOCUMENT_NEEDS_REBUILD_CLOSE_REASON,
  DOCUMENT_REBUILT_CLOSE_REASON,
  DOCUMENT_SCHEMA_CLOSE_CODE,
  DOCUMENT_TOO_LARGE_CLOSE_CODE,
  DOCUMENT_TOO_TANGLED_CLOSE_REASON,
  DOCUMENT_UPDATE_REFUSED_CLOSE_REASON,
} from "@legion/contracts";
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
    return undefined;
  }
  // The socket is up and this admission can send, yet what this browser holds has sat there
  // longer than a frame's round trip without the server taking one (`UNSENT_NOTICE_MS`): the
  // handover or the acknowledgements have stopped. The person is told the edits are kept and
  // still going out, which is what they can act on - keep the tab open - rather than why.
  if (connection === "connected" && pending.unsent && pending.count > 0) {
    return `${savedEdits(pending.count)} saved in this browser, not sent to Dispatch yet. Dispatch keeps trying.`;
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

/**
 * What the document says while its next socket waits on the connection's backoff
 * (`ConnectionCallbacks.onReconnectWait`): that it is reconnecting, in `secondsLeft` seconds when
 * that is given, and what this browser keeps of the person's typing meanwhile. A read-only
 * admission takes no typing, and a document whose saved rows are not known yet promises nothing.
 */
export function reconnectingNotice(
  pending: PendingState | undefined,
  secondsLeft: number | undefined = undefined
): string {
  const waiting =
    secondsLeft === undefined
      ? "Reconnecting to the document."
      : `Reconnecting to the document in ${secondsLeft} s.`;
  if (pending === undefined || pending.readOnly) {
    return waiting;
  }
  return pending.stored
    ? `${waiting} Your edits are kept in this browser until they are sent.`
    : `${waiting} Your edits stay on this page, but a reload would lose them.`;
}

/**
 * The sentence the editor shows for an edit the server refused with
 * `DOCUMENT_TOO_LARGE_CLOSE_CODE`, by the close's reason: an edit the server could not merge into
 * the document quickly enough (`DOCUMENT_TOO_TANGLED_CLOSE_REASON`), one the document cannot take
 * until it is rebuilt (`DOCUMENT_NEEDS_REBUILD_CLOSE_REASON`: its edit history has used up what the
 * server lets it scan), one this browser typed against the history someone else's rebuild replaced
 * (`DOCUMENT_REBUILT_CLOSE_REASON`), one holding content the document does not take
 * (`DOCUMENT_UPDATE_REFUSED_CLOSE_REASON`), one made under a client id Dispatch did not give this
 * browser's person (`DOCUMENT_CLIENT_ID_REFUSED_CLOSE_REASON`), or one that would make the
 * document too large. Every one of them is permanent: the refused edit is dropped and the
 * document re-read, so a reconnect cannot meet the same refusal again.
 *
 * The rebuilt document's notice promises no other edit is kept, because none typed against the
 * replaced history is: the restore drops each of those rows and reports when they were saved
 * (`pending-sync.ts`, `addsParking`). The client id refusal names how many edits went, `dropped`:
 * every saved row under the refused id goes with the refused edit, since each would meet the same
 * refusal (`dropClientRows`), so its notice says no more than that. Where none went, the notice
 * names no edit as lost and says only that what reached Dispatch is kept: the browser held no
 * saved row under that id, as when every edit the page made under it had reached Dispatch before a
 * later socket would not give the id back (`takeClientId`). A browser that keeps no rows
 * (`PendingState.stored`) counts none either, whatever its document held unsent. Of these refusals
 * the page offers a rebuild only for `DOCUMENT_NEEDS_REBUILD_CLOSE_REASON`, the one a rebuild
 * helps (`ProofDocument`); a rebuild of a document just rebuilt is answered 409.
 */
export function refusedEditNotice(reason: string, dropped?: number): string {
  switch (reason) {
    case DOCUMENT_TOO_TANGLED_CLOSE_REASON:
      return "Your last edit was not saved: Dispatch could not merge it into this document quickly enough. Your other edits are kept.";
    case DOCUMENT_NEEDS_REBUILD_CLOSE_REASON:
      return "This document has been edited so much that it must be rebuilt before it can take this edit. Your other edits are kept.";
    case DOCUMENT_REBUILT_CLOSE_REASON:
      return "Someone else rebuilt this document, so the edits you had not sent could not be kept. Dispatch has reloaded the rebuilt document.";
    case DOCUMENT_UPDATE_REFUSED_CLOSE_REASON:
      return "Your last edit was not saved: it holds content this document does not take.";
    case DOCUMENT_CLIENT_ID_REFUSED_CLOSE_REASON: {
      if (dropped === 0) {
        return "Dispatch has reloaded the document: this page had it open under an editing identity Dispatch did not give you. Every edit of yours that reached Dispatch is kept.";
      }
      const count = dropped ?? 1;
      return count === 1
        ? "1 of your edits was not saved: it was made under an editing identity Dispatch did not give you, so it cannot be sent. Dispatch has reloaded the document."
        : `${count} of your edits were not saved: they were made under an editing identity Dispatch did not give you, so they cannot be sent. Dispatch has reloaded the document.`;
    }
    default:
      return "Your last edit was not saved: it would make this document larger than a Dispatch document may be. Your other edits are kept.";
  }
}

export interface DocumentConnection {
  readonly awareness: Awareness;
  readonly doc: Doc;
  destroy(): void;
  /**
   * Has the server decide a read-only admission again: the server decides a socket's admission
   * once, as it connects, so this closes a socket admitted read-only, the connection reconnects as
   * it does after any close the server makes, and the new socket's admission reaches
   * `onAdmission` and the pending sync. A socket admitted writable keeps its admission. A socket
   * the server has not answered yet may be one the connection opened after the server's own
   * close, which the server admits as it decides now, so its answer decides: a read-only one
   * closes it, unreported. Closing that socket regardless would cost a second reconnect, and its
   * draw. Either close is this page's own and says nothing of the server, so its reconnect waits
   * the first attempt's spread whatever backoff earlier closes built.
   */
  readmit(): void;
  /**
   * Sends the socket the connection is waiting to reconnect (`onReconnectWait`) now, rather than
   * when its backoff says, and does nothing while no reconnect waits. It brings that one attempt
   * forward and resets nothing: the attempt was counted as its wait began, a socket it opens that
   * closes within `STEADY_SOCKET_MS` waits the next attempt's backoff, and only a socket that
   * stays up that long starts the backoff over, as after any other attempt. The wait ends as the
   * socket is made, so one call opens at most one socket, and another waits for that one to close.
   */
  reconnectNow(): void;
}

/** A wait before the connection's next socket (`ConnectionCallbacks.onReconnectWait`). */
export interface ReconnectWait {
  /** When the next socket goes out, by `Date.now()`. */
  readonly at: number;
  /** The reconnect attempt it is: 0 the first after a socket that stayed up `STEADY_SOCKET_MS`
   *  or a readmission, and one more for each socket in a row that closed sooner. */
  readonly attempt: number;
}

export interface ConnectionCallbacks {
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
  /**
   * Fires when the server refused this browser's edit with `DOCUMENT_TOO_LARGE_CLOSE_CODE`, with
   * the close's reason: the edit would take the document past its size bound, the server could
   * not merge it quickly enough (`DOCUMENT_TOO_TANGLED_CLOSE_REASON`), the document must be
   * rebuilt before it can take it (`DOCUMENT_NEEDS_REBUILD_CLOSE_REASON`), the edit was typed
   * against a history someone else's rebuild replaced (`DOCUMENT_REBUILT_CLOSE_REASON`), it
   * holds content the document does not take (`DOCUMENT_UPDATE_REFUSED_CLOSE_REASON`), or it was
   * made under a client id Dispatch did not give this browser's person
   * (`DOCUMENT_CLIENT_ID_REFUSED_CLOSE_REASON`). It fires with that last reason too when the
   * server will not give back the client id this connection's document has written under (the
   * socket's first frame, `readClientIdMessage`), and with `DOCUMENT_REBUILT_CLOSE_REASON` when a
   * socket closes any other way while this connection's document holds the history a rebuild
   * replaced (`holdsReplacedHistory`). The
   * connection has stopped reconnecting and the refused edit's saved row is gone - for the client
   * id refusal, every saved row under the refused id, `dropped` of them (`dropClientRows`); the
   * refused text is still in this connection's document, so the host rebuilds it from a fresh read
   * rather than let a reconnect send it again.
   */
  onEditRefused(reason: string, dropped?: number): void;
  /**
   * Fires `true` when the server closed the socket because the memory it keeps for open documents
   * is full (`DOCUMENT_BUSY_CLOSE_CODE`), and `false` once it applies a frame from this browser
   * again. The connection keeps reconnecting on its backoff, as after any close that ends no
   * refusal, and the edits it could not send stay in this document and in the browser's saved rows
   * until a socket takes them.
   */
  onBusy(waiting: boolean): void;
  /**
   * Fires with the wait as the connection starts waiting to reconnect after a close that ends no
   * refusal, and with `undefined` once the socket it waited for is made or the connection is
   * destroyed. The provider's own reconnect after its silent-socket check is no wait of this
   * connection's and fires nothing (`reconnectWaitMs(0)` at a socket's open).
   */
  onReconnectWait(wait: ReconnectWait | undefined): void;
  onStatus(state: ConnectionState): void;
  /** Reports browser-held edits as IndexedDB changes or this admission's restore completes. */
  onPending(state: PendingState): void;
  /** Fires once, when the server's first sync completes after admission. */
  onSynced(): void;
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

/**
 * How long a socket must have stayed open for its close to start the connection's backoff over
 * (`reconnect`, below). Every close sooner than this waits the next attempt's backoff, so a pod
 * that admits the page and then dies, or turns away with 4503 the edit each reconnect brings,
 * hears from each tab less and less often. Half a minute, because it is:
 * - twice the server's `WriteWaitBound` (15 s, packages/envoy/internal/dispatch/docs/livewrite.go),
 *   the longest the server keeps an admitted socket waiting before it closes it busy
 *   (`weighStateAnswer`), so no socket a full pod turns away counts as having stayed up;
 * - the backoff's own cap, so a loop whose sockets each outlast it still reconnects no more often
 *   than once in 30 s, the rate the capped backoff holds a dead pod to;
 * - the provider's `messageReconnectTimeout`, the silence after which it takes a socket for dead.
 * What it costs: a person whose link drops more often than every 30 s waits longer after each
 * drop, 15 to 30 s from the sixth on, where every drop used to be back within 2 s; a link that
 * stays up 30 s between drops is back within the 2 s spread every time.
 */
const STEADY_SOCKET_MS = 30_000;

/** The provider never reconnects after this close, its `Forbidden`: its close handler returns
 *  before it either reconnects or fails the attempt (@hocuspocus/provider 2.15.3,
 *  dist/hocuspocus-provider.esm.js:1987-1992). */
const PROVIDER_FORBIDDEN_CLOSE_CODE = 4403;

/**
 * The provider's `initialDelay` for reconnect attempt `attempt`, 0 the first after a socket that
 * stayed up: `reconnectDelayMs` in whole milliseconds, the only delay the provider takes, and never
 * 0, at which the provider's own reconnect would open its socket before `reconnect` can replace it.
 */
function reconnectWaitMs(attempt: number): number {
  return Math.max(1, Math.floor(reconnectDelayMs(attempt)));
}

// Hocuspocus and Yjs load with the first document that connects, not with the issue route: the
// same lazy boundary `createEditor` puts around the editor itself. The connect they resolve to is
// synchronous, so a host has one place to react to the connection.
export async function loadDocumentTransport(): Promise<ConnectDocument> {
  const [
    { HocuspocusProvider, HocuspocusProviderWebsocket },
    { Doc },
    { Awareness: Presence },
    { openPendingEdits },
    { holdsReplacedHistory, startPendingSync },
    { readClientIdMessage },
  ] = await importWhenOnline(() =>
    Promise.all([
      import("@hocuspocus/provider"),
      import("yjs"),
      import("y-protocols/awareness"),
      import("./pending-edits"),
      import("./pending-sync"),
      import("./sync-frame"),
    ])
  );
  return (artifactId, callbacks) => {
    // The document starts with the id Yjs drew for it, which it never writes or announces under:
    // the first frame of its first socket gives it the one Dispatch gave this page
    // (`takeClientId`), and every later socket presents that id back and is given it again. So
    // the same document writes under one client id across every reconnect, its offline edits
    // included, and only a new document - the host's rebuild after a refusal - takes a fresh one.
    // A later socket that gives another id moves the document to it only while it has written
    // nothing under the one it holds.
    const doc = new Doc();
    // y-protocols makes an awareness with an empty state under the document's id. That id is not
    // the one this document will write under, so the state is taken back before the provider
    // listens - the provider queues what its awareness emits for the socket it has yet to open -
    // and set again under the id the first socket gives (`takeClientId`).
    const presence = new Presence(doc);
    presence.setLocalState(null);
    // `clientId` settles as the document takes its first id, which its saved rows are keyed by
    // from then on, under whatever id a later socket moves it to (`PendingEdits.keyed`);
    // `socketAnswered` is whether the current socket has delivered its first frame, the one that
    // names that id.
    const clientId = Promise.withResolvers<number>();
    let socketAnswered = false;
    let destroyed = false;
    // Whether any admission has been handed to the pending sync: the first one's restore ends in
    // the host's `onSynced`.
    let announced = false;
    // This admission's restore of the browser's saved rows; a refused edit is dropped after it.
    let restored: Promise<void> = Promise.resolve();
    // The server last closed a socket for want of room, and has applied nothing from this browser
    // since. `busyCloses` counts those closes, so an applied answer clears `busy` only when no
    // busy close came after the answer arrived: the pending sync answers once its saved row is
    // cleared, which is asynchronous, and a close that lands meanwhile is the newer word.
    let busy = false;
    let busyCloses = 0;
    // The attempt the next reconnect is, from 0, the first after a socket that stayed up
    // `STEADY_SOCKET_MS`; `openedAt` is when the current socket opened, by the page's clock, and
    // undefined while none is open; `readmitting` says the page closed it to be admitted again
    // (`readmit`, or a read-only answer on the socket a readmit found unanswered; `reconnect`).
    // `waiting` is the wait the host was told of, until the socket it waits for is made.
    let attempt = 0;
    let openedAt: number | undefined;
    let readmitting = false;
    let waiting: ReconnectWait | undefined;
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
    const WebSocketPolyfill = class extends globalThis.WebSocket {
      constructor(url: string, protocols?: string | string[]) {
        super(url, protocols);
        // The socket's first frame is the one that names this document's client id.
        socketAnswered = false;
        // The socket the host was told to expect is on the wire: no reconnect waits now.
        if (waiting !== undefined) {
          waiting = undefined;
          callbacks.onReconnectWait(undefined);
        }
        this.addEventListener("open", () => {
          openedAt = performance.now();
          // The page's first connect waits nothing: the provider's `initialDelay` is 0 until a
          // socket opens. After that, the one reconnect `reconnect` does not schedule is the
          // provider's own when its silent-socket check gives up on a socket whose close never
          // fired: it calls its close handler with no close event, the 4408 of
          // dist/hocuspocus-provider.esm.js:1923-1930, so the connection never hears of it. That
          // socket carried a frame and then 30 s of nothing, so it stayed up, and this is the wait
          // `reconnect` would give it: a fresh draw of the first attempt's spread.
          websocket.setConfiguration({ initialDelay: reconnectWaitMs(0) });
          sync?.socketOpened();
        });
      }

      override send(data: Parameters<WebSocket["send"]>[0]): void {
        super.send(data);
        if (data instanceof Uint8Array) {
          sync?.frameWritten(data);
        }
      }
    };
    // Nothing here ends a socket that waits for its room. The server answers the upgrade only
    // once it has admitted the browser and loaded the document, which a burst has kept a browser
    // waiting 21.4 s for on the copy bound alone (doc.e2e.ts holds one 25 s). Until the first
    // frame arrives the provider is `connecting`: its `messageReconnectTimeout` check (30 s) skips
    // a socket that is not connected or has received nothing, and its retry has no `timeout`
    // (@hocuspocus/provider 2.15.3, dist/hocuspocus-provider.esm.js:1676,1692,1903-1914). Any
    // timeout added to this path must outlast that wait, or the reconnect queues the browser again.
    //
    // The server decides admission from these when it accepts the socket. A server that predates
    // `schema_creatable` ignores that parameter, and one that predates client ids ignores
    // `DOCUMENT_CLIENT_ID_PARAMETER`: the client id this document holds, empty until a socket has
    // given it one (`takeClientId`). The provider reads these afresh for each socket it opens.
    const parameters: Record<string, string | number> = {
      [DOCUMENT_CLIENT_ID_PARAMETER]: "",
      schema_creatable: callbacks.schemaCreatable,
      schema_version: callbacks.schemaVersion,
    };
    const websocket = new HocuspocusProviderWebsocket({
      WebSocketPolyfill,
      connect: false,
      parameters,
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
    // no-op all the same. Each admission's restore is kept as `restored`, which a refusal of this
    // browser's edit waits for before it drops the refused row (`dropRefused`).
    const notifyAdmitted = () => {
      if (!provider.isSynced || !provider.isAuthenticated) {
        return;
      }
      const restoring = Promise.resolve(sync?.synced()).catch((error: unknown) => {
        console.error("Could not restore this document's pending browser edits", error);
      });
      restored = restoring;
      if (announced) {
        return;
      }
      announced = true;
      void restoring.then(() => {
        if (!destroyed) {
          callbacks.onSynced();
        }
      });
    };
    // Ends this connection for an edit the server refused, and tells the host why once the
    // refused frame's saved row is gone: for the client id refusal, every saved row under the
    // refused id, or under `clients` where the caller names the ids itself (`dropClientRows`).
    // The provider's own close handler, which runs before the connection's, has already started a
    // reconnect that this disconnect ends.
    const refuseEdit = (reason: string, clients?: readonly number[]) => {
      // The refused frame is taken now, from the socket the server has just closed.
      const dropping: Promise<number | undefined> =
        reason === DOCUMENT_CLIENT_ID_REFUSED_CLOSE_REASON
          ? Promise.resolve(sync?.dropClientRows(restored, clients))
          : Promise.resolve(sync?.dropRefused(restored)).then(() => undefined);
      void dropping.then((dropped) => {
        if (!destroyed) {
          callbacks.onEditRefused(reason, dropped);
        }
      });
      provider.disconnect();
    };
    // Takes the client id the current socket's first frame names (`readClientIdMessage`), or
    // keeps the one the document holds when that frame names none, as a server of a build from
    // before Dispatch gave client ids out sends none. The id is the document's own until it has
    // written under it: a document that has not - the first socket's, and a later one's that has
    // written nothing yet - takes the id it is given, with its awareness, whose client id
    // y-protocols copied from the document when this connection made it (`new Presence(doc)`,
    // above). One that has, and is given another id, holds edits the server will not take as this
    // person's: it is refused as such an edit is, and the host rebuilds it under a fresh id. Only
    // the first id it takes settles `clientId`, so its saved rows stay keyed by that one whatever
    // id a later socket moves it to, and the pending sync reads them as this page's own by that
    // key (`otherClientsRows`). Its presence is announced only from here (the provider's own
    // announcement at each socket's open sends nothing while the local state is null), so no other
    // reader ever sees it under an id the server did not give it.
    const takeClientId = (given: number | undefined) => {
      const held = doc.clientID;
      if (given !== undefined && given !== held) {
        if (doc.store.clients.has(held)) {
          refuseEdit(DOCUMENT_CLIENT_ID_REFUSED_CLOSE_REASON, [held]);
          return;
        }
        // A state announced under the id the document held is taken back under it first; the
        // first socket's document announced none.
        const state = presence.getLocalState();
        if (state !== null) {
          presence.setLocalState(null);
        }
        doc.clientID = given;
        presence.clientID = given;
        presence.setLocalState(state ?? {});
      } else if (presence.getLocalState() === null) {
        presence.setLocalState({});
      }
      parameters[DOCUMENT_CLIENT_ID_PARAMETER] = String(doc.clientID);
      clientId.resolve(doc.clientID);
    };
    // Decides when the next socket goes out after every close that ends no refusal: a socket the
    // server answered and one that failed before any answer alike, `DOCUMENT_BUSY_CLOSE_CODE`
    // included. The provider's own close handler has run first and started a reconnect of its own:
    // after an answered socket a fresh `connect()`, which waits `initialDelay`, and after one that
    // was never answered the next try of that `connect()`'s retry, on its own backoff (1 s, then a
    // draw between 1 s and a doubling ceiling of up to 30 s; dist/hocuspocus-provider.esm.js:
    // 1676-1692,1773-1817,1997-2004 with @lifeomic/attempt 3.1.0). That backoff lives inside one
    // `connect()`, and a socket's first frame ends it, so a pod that admits the page and dies, or
    // turns its edit away with 4503, had every tab back within the 2 s spread each time, for ever
    // (dispatch://INFRA-179). The `connect()` here cancels whatever the provider started, whose
    // retry then aborts before its next socket, and starts the one try this backoff chooses, so
    // the provider's own backoff never opens a socket.
    //
    // The backoff is the live socket's (`reconnectDelayMs`, api/live-socket.ts), so a tab's two
    // sockets to one pod come back on one schedule. The first attempt is spread over [0, 2 s): a
    // blip is back within 2 s, a second on average, and a restart's crowd of tabs reaches the new
    // process over two seconds rather than at once. Each later one doubles from 1 s and is capped
    // at 30 s, [1, 2) s, [2, 4) s up to [15, 30) s from the sixth: a dead pod hears from a tab at
    // most every 15 s, and a document whose pod returns is back within 30 s of it. The jitter is
    // equal, half the ceiling plus a draw of the other half: Hocuspocus draws between 1 s and the
    // ceiling, so even at its cap some tabs come back after a second, where here none waits less
    // than half of it.
    const reconnect = () => {
      const lived = openedAt === undefined ? 0 : performance.now() - openedAt;
      openedAt = undefined;
      if (readmitting || lived >= STEADY_SOCKET_MS) {
        attempt = 0;
      }
      readmitting = false;
      const wait = reconnectWaitMs(attempt);
      websocket.setConfiguration({ initialDelay: wait });
      // Told before the wait starts, and taken back as the socket it waits for is made, which
      // `connect()` does from inside this call when `reconnectNow` has cut the wait to nothing.
      waiting = { at: Date.now() + wait, attempt };
      attempt += 1;
      callbacks.onReconnectWait(waiting);
      void websocket.connect();
    };
    provider = new HocuspocusProvider({
      awareness: presence,
      document: doc,
      name: artifactId,
      onAuthenticated: () => {
        const readOnly = isSchemaReadOnly(provider.authorizedScope);
        const socket = websocket.webSocket;
        if (socket !== null && socket === readmitOnAnswer) {
          readmitOnAnswer = undefined;
          if (readOnly) {
            // A read-only answer on a socket the readmit found unanswered: the connection
            // reconnects, and the next socket's admission is the one the host is told of. The
            // close is the page's own, as `readmit`'s is (`reconnect`).
            readmitting = true;
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
        // Every other close reconnects, on this connection's backoff (`reconnect`), which is what
        // the transient `DOCUMENT_BUSY_CLOSE_CODE` wants: the server is out of room for open
        // documents, and a later attempt finds what it has freed, so the edits it refused wait to
        // go with it. The other two are the server's decision about the document itself, so the
        // connection stops: a document outside the Proof schema the host reads again, and an edit
        // past the document's size bound, one the server could not merge quickly enough, one the
        // document cannot take until it is rebuilt and one typed against a history someone else's
        // rebuild replaced, is dropped before the host rebuilds the document without it.
        switch (event.code) {
          case DOCUMENT_SCHEMA_CLOSE_CODE:
            provider.disconnect();
            callbacks.onOutsideSchema();
            return;
          case DOCUMENT_TOO_LARGE_CLOSE_CODE:
            refuseEdit(event.reason);
            return;
        }
        // A document that holds the history a rebuild replaced never connects again, whatever
        // closed its socket: the rebuilt room refuses that history as `DOCUMENT_REBUILT_CLOSE_REASON`
        // and closes the socket at once, and a close frame the connection's reset overtakes reaches
        // the page as an abnormal closure (1006, no reason) - in Chromium, 3 of 39 runs of the
        // used-up row (e2e/document-refusals.e2e.ts). The room sent its state before that refusal,
        // its list of the clients the rebuild retired with it, and this document took both: that is
        // how it knows. The provider's reconnect would hand the same history back and be refused
        // again, so the connection ends as that refusal ends it, and the host reads the rebuilt
        // document afresh.
        if (holdsReplacedHistory(doc)) {
          refuseEdit(DOCUMENT_REBUILT_CLOSE_REASON);
          return;
        }
        if (event.code === DOCUMENT_BUSY_CLOSE_CODE) {
          busyCloses += 1;
          if (!busy) {
            busy = true;
            callbacks.onBusy(true);
          }
        }
        // The provider stops for good after the closes it takes as final - unauthorized (4401),
        // a message too big (1009), forbidden - and after this connection's own disconnect; the
        // backoff reconnects only where the provider would have.
        if (destroyed || !websocket.shouldConnect || event.code === PROVIDER_FORBIDDEN_CLOSE_CODE) {
          return;
        }
        reconnect();
      },
      onMessage: ({ event }) => {
        if (!(event.data instanceof ArrayBuffer)) {
          return;
        }
        const frame = new Uint8Array(event.data);
        // The provider hands the frame on only after this returns, so the document holds the id
        // the socket gives it before it answers the server's SyncStep1 that follows.
        if (!socketAnswered) {
          socketAnswered = true;
          takeClientId(readClientIdMessage(frame));
        }
        // The pending sync parses the frame, which is the one parse of it: it answers whether the
        // server applied the frame it acknowledges, and `undefined` for anything else. It answers
        // after it has cleared the acknowledged row, so a busy close that arrived meanwhile -
        // after this frame, as the socket delivered them - keeps the notice up.
        const closesBefore = busyCloses;
        void Promise.resolve(sync?.frameReceived(frame)).then((applied) => {
          if (busy && applied === true && busyCloses === closesBefore) {
            busy = false;
            callbacks.onBusy(false);
          }
        });
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
    sync = startPendingSync({
      doc,
      onChange: callbacks.onPending,
      remoteOrigin: provider,
      // The store opens at once, so the rows earlier pages saved are named while no socket has
      // answered yet - a reload with the network down shows them. The rows this document saves
      // are keyed by the first id it takes, which its first socket gives it (`clientId`); no edit
      // is typed before then, since the editor is made only once that socket has synced
      // (`onSynced`).
      store: openPendingEdits(artifactId, clientId.promise),
    });
    void websocket.connect();
    return {
      awareness: presence,
      destroy() {
        if (destroyed) {
          return;
        }
        destroyed = true;
        if (waiting !== undefined) {
          waiting = undefined;
          callbacks.onReconnectWait(undefined);
        }
        sync?.destroy();
        provider.destroy();
        websocket.destroy();
        doc.destroy();
      },
      doc,
      readmit() {
        const socket = websocket.webSocket;
        // With no socket, the connection's next one connects after this call.
        if (destroyed || socket === null) {
          return;
        }
        if (admitted === undefined || admitted.socket !== socket) {
          readmitOnAnswer = socket;
          return;
        }
        if (admitted.readOnly) {
          readmitting = true;
          socket.close();
        }
      },
      reconnectNow() {
        if (destroyed || waiting === undefined) {
          return;
        }
        // The attempt this wait belongs to is already counted, so this opens its socket early and
        // leaves the backoff where it stands. The `connect()` cancels the retry waiting out the
        // old delay and starts one that waits a millisecond; the socket it makes takes the wait
        // back, so a second call before that socket exists only draws the same millisecond again.
        websocket.setConfiguration({ initialDelay: 1 });
        void websocket.connect();
      },
    };
  };
}
