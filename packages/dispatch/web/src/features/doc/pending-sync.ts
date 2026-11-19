import { MessageType } from "@hocuspocus/provider";
import { createDecoder, readVarUint } from "lib0/decoding";
import { ySyncPluginKey } from "y-prosemirror";
import { messageYjsSyncStep2, messageYjsUpdate } from "y-protocols/sync";
import * as Y from "yjs";

import type { PendingEdits, PendingKey, RestoredEdit } from "./pending-edits";
import { RETIRED_MAP_NAME } from "./roots";
import { readField } from "./sync-frame";

export interface PendingState {
  /** Rows this document holds across every tab in this browser. */
  count: number;
  /** This admission cannot send the rows it holds. */
  readOnly: boolean;
  /** When the oldest dropped row was saved before a document rebuild. */
  rebuiltAt: number | undefined;
  /** Whether this browser has an IndexedDB store for these edits. */
  stored: boolean;
  /**
   * Whether the rows this browser holds have waited longer than `UNSENT_NOTICE_MS` for the
   * server to take one of its frames: the edits are saved here and have not reached Dispatch.
   */
  unsent: boolean;
}

/**
 * How long the rows this browser holds may wait for the server to take one of its frames before
 * a person is told the edits have not reached Dispatch. One frame's round trip on this pod is
 * 31 ms at the median of a keystroke, 176 ms at the 90th percentile and 539 ms at the 99th, and
 * no single frame a room sends is larger than 64 KiB, so two seconds is close to four of the
 * slowest round trips and longer than the wait any one frame costs: typing never reaches it, and
 * a handover or an acknowledgement that has stopped always does. The wait starts over every time
 * the server takes a frame, so a person typing steadily is never named either.
 */
export const UNSENT_NOTICE_MS = 2_000;

export interface PendingSync {
  /** Starts a fresh acknowledgement sequence for a newly opened socket. */
  socketOpened(): void;
  /** Counts a SyncStep2 or Update frame that actually reached the socket. */
  frameWritten(frame: Uint8Array): void;
  /**
   * Pairs an incoming SyncStatus frame with the next counted sent frame, and reports the flag it
   * read: whether the server applied the frame it acknowledges, and `undefined` for a frame that
   * is no SyncStatus, one that does not parse and one this sync is destroyed for. It is the one
   * parse of the frame, so a caller watching for the server to take a frame again reads it here
   * (`connection.ts`, `onBusy`).
   */
  frameReceived(frame: Uint8Array): Promise<boolean | undefined>;
  /**
   * Deletes the saved row of the Update frame this socket's server last answered as not applied,
   * once `restored` - this admission's restore (`synced`) - has settled, so the rows that restore
   * replaced are gone before the document is rebuilt. The frame is taken when this is called, from
   * the socket the server has just closed with `DOCUMENT_TOO_LARGE_CLOSE_CODE` after that answer:
   * the edit would take the document past its size bound, so resending it can only earn the same
   * refusal. A plain not-applied answer drops nothing, which is what keeps a read-only admission's
   * rows.
   */
  dropRefused(restored: Promise<void>): Promise<void>;
  /**
   * Deletes every saved row - this tab's and every other tab's - whose edit is under one of
   * `clients`, or, where none are named, under a client of the Update frame this socket's server
   * last answered as not applied, once `restored` has settled, and answers how many saved rows it
   * deleted. The server refused that frame with `DOCUMENT_CLIENT_ID_REFUSED_CLOSE_REASON`: the
   * client id it writes under is not this person's, so every other row under that id would meet
   * the same refusal, one reconnect at a time, and they go together. Rows under other ids stay,
   * and the rebuilt document replays them as it replays any row.
   */
  dropClientRows(restored: Promise<void>, clients?: readonly number[]): Promise<number>;
  /**
   * Takes the current socket's admission as it authenticates: whether it is read-only decides from
   * then on whether a local update is a person's edit to store. Restoring the stored rows waits
   * for `synced`.
   */
  authenticated(readOnly: boolean): void;
  /**
   * Takes the current socket's admission once that socket has both synced and authenticated. The
   * first, and each later one whose read-only flag differs from the last one taken, decides what
   * the stored rows need: a writable admission replays them, each sent again and kept until an
   * applied answer covers it, whatever of it this document already holds; a read-only one keeps
   * them all for the next writable admission.
   */
  synced(): Promise<void>;
  destroy(): void;
}

interface RecordedUpdate {
  ordinal: number;
  seq: number | undefined;
  update: Uint8Array;
  written: Promise<void>;
}

type SentFrame = { kind: "update"; update: Uint8Array } | { kind: "step2"; through: number };

export interface PendingSyncOptions {
  doc: Y.Doc;
  onChange(state: PendingState): void;
  /** Updates received from the provider must never be stored as local edits. */
  remoteOrigin: unknown;
  /**
   * Sends `update` to the server as an Update frame on the current socket. A restored row is
   * applied as the server's own update, which the provider does not send, and sent whole through
   * this, so the answer that clears it answers its own bytes (`admit`).
   */
  send(update: Uint8Array): void;
  /** Opening IndexedDB must not delay constructing the provider. */
  store: Promise<PendingEdits | undefined>;
}

function equals(left: Uint8Array, right: Uint8Array): boolean {
  return left.length === right.length && left.every((byte, index) => byte === right[index]);
}

/**
 * The flag of a SyncStatus frame: true when the server applied the frame it answers, false when it
 * did not, and undefined for any other frame. Throws for a frame that does not parse.
 */
export function readSyncStatus(frame: Uint8Array): boolean | undefined {
  const decoder = createDecoder(frame);
  readField(decoder); // the document name
  if (readVarUint(decoder) !== MessageType.SyncStatus) {
    return undefined;
  }
  // The server writes the flag as a VarUint: 1 applied, 0 not applied.
  return readVarUint(decoder) === 1;
}

function covers(
  ranges: readonly { clock: number; len: number }[],
  clock: number,
  len: number
): boolean {
  let cursor = clock;
  const end = clock + len;
  for (const range of ranges) {
    if (range.clock > cursor) {
      return false;
    }
    cursor = Math.max(cursor, range.clock + range.len);
    if (cursor >= end) {
      return true;
    }
  }
  return false;
}

/** The clients of the parked deletes applying an update added that the live document did not hold. */
function addedPendingDeletes(before: Uint8Array | null, after: Uint8Array | null): number[] {
  if (after === null) {
    return [];
  }
  const beforeDeletes =
    before === null
      ? new Map<number, { clock: number; len: number }[]>()
      : Y.decodeUpdateV2(before).ds.clients;
  const added: number[] = [];
  for (const [client, afterRanges] of Y.decodeUpdateV2(after).ds.clients) {
    const beforeRanges = beforeDeletes.get(client) ?? [];
    if (afterRanges.some((range) => !covers(beforeRanges, range.clock, range.len))) {
      added.push(client);
    }
  }
  return added;
}

/**
 * A copy of `doc` to test restored rows on, so the live document never takes a row typed against
 * a history a rebuild replaced.
 */
function scratchOf(doc: Y.Doc): Y.Doc {
  const scratch = new Y.Doc();
  Y.applyUpdate(scratch, Y.encodeStateAsUpdate(doc));
  return scratch;
}

/**
 * Applies a restored row to `scratch` and answers the clients it parked on that the scratch did not
 * already park on: a client whose content its structs build on and the scratch lacks, or whose
 * items it deletes and the scratch does not hold. A row typed against a history the server rebuilt
 * since parks on that history's clients; one typed after another person's edit the server never
 * stored parks on that person's. yjs 13.6.32 exposes parked structs and deletes only through these
 * `StructStore` internals. `encodeStateAsUpdate` carries the live document's own parked structs and
 * deletes into the scratch, so only what the row adds counts.
 */
function parkedClients(scratch: Y.Doc, update: Uint8Array): Set<number> {
  const beforeMissing = new Set(scratch.store.pendingStructs?.missing.keys() ?? []);
  const beforeDeletes = scratch.store.pendingDs;

  Y.applyUpdate(scratch, update);

  const parked = new Set<number>();
  for (const client of scratch.store.pendingStructs?.missing.keys() ?? []) {
    if (!beforeMissing.has(client)) {
      parked.add(client);
    }
  }
  for (const client of addedPendingDeletes(beforeDeletes, scratch.store.pendingDs)) {
    parked.add(client);
  }
  return parked;
}

/**
 * The clients a rebuild of `doc`'s document retired, each with the clock it had reached in the
 * history the rebuild replaced, as the rebuilt history lists them (`RETIRED_MAP_NAME`), read as the
 * server reads the list (`retiredClientsOf`): an entry that is not a client and a clock of at least
 * one retires nothing. Empty for a document no rebuild has replaced. The list is asked for by its
 * class only once the document holds it, so a live document is never given a root it lacks.
 */
export function retiredClients(doc: Y.Doc): Map<number, number> {
  const retired = new Map<number, number>();
  if (!doc.share.has(RETIRED_MAP_NAME)) {
    return retired;
  }
  doc.getMap(RETIRED_MAP_NAME).forEach((clock, key) => {
    const client = Number(key);
    if (Number.isSafeInteger(client) && client >= 0 && typeof clock === "number" && clock >= 1) {
      retired.set(client, clock);
    }
  });
  return retired;
}

/**
 * Whether `doc` holds the history a rebuild of its document replaced: a struct of a client the
 * rebuilt history retired. A client's structs run from clock 0, below any clock the list names,
 * and the rebuilt room holds none of them, so this document's answer to the room's SyncStep1
 * would carry them and the room would refuse it (`DOCUMENT_REBUILT_CLOSE_REASON`). A document
 * built afresh from the rebuilt room holds the list and none of those clients.
 */
export function holdsReplacedHistory(doc: Y.Doc): boolean {
  for (const client of retiredClients(doc).keys()) {
    if (Y.getState(doc.store, client) > 0) {
      return true;
    }
  }
  return false;
}

/**
 * Whether a local transaction is the editor's repair of the document, not an edit. y-prosemirror
 * (1.3.7, `sync-plugin.js`) deletes an element or text it cannot build with the editor's schema
 * (`createNodeFromYElement`, `createTextNodesFromYText`): an element a newer build wrote, for one.
 * It makes that deletion in a transaction of its own whose origin is its sync plugin key. An edit
 * typed in the editor has the same origin, but its transaction always carries the `addToHistory`
 * meta, which the sync plugin's view update sets.
 */
function isEditorRepair(origin: unknown, transaction: Y.Transaction): boolean {
  return origin === ySyncPluginKey && !transaction.meta.has("addToHistory");
}

/**
 * The attributes the editor derives from the document rather than takes from a person, by the node
 * that carries them. Milkdown's `syncHeadingIdPlugin` slugs every heading's id from its text as it
 * loads the document, and its `syncListOrderPlugin` labels every ordered list item with its number
 * and types it as ordered on the first caret move or edit. No rendering carries either, so the
 * server settles an update that changes only these without a version (envoy's
 * `TestAttributesTheEditorDerivesSettleWithoutAVersion`), and the next load derives them again.
 */
const DERIVED_ATTRIBUTES: Record<string, readonly string[]> = {
  heading: ["id"],
  list_item: ["label", "listType"],
};

/**
 * Whether a transaction changed nothing but `DERIVED_ATTRIBUTES`. Yjs lists each type that existed
 * before the transaction and that it changed, with the attribute keys it set or removed and `null`
 * for a change to the type's children or text. A type the transaction created is inserted into one
 * that existed, so it shows as that parent's `null`.
 */
function changesOnlyDerived(transaction: Y.Transaction): boolean {
  if (transaction.changed.size === 0) {
    return false;
  }
  for (const [type, keys] of transaction.changed) {
    // `Object.hasOwn`, not indexing alone: the table inherits `Object.prototype`, so a node named
    // `constructor` would read a function.
    const attributes =
      type instanceof Y.XmlElement && Object.hasOwn(DERIVED_ATTRIBUTES, type.nodeName)
        ? DERIVED_ATTRIBUTES[type.nodeName]
        : undefined;
    if (attributes === undefined) {
      return false;
    }
    for (const key of keys) {
      if (key === null || !attributes.includes(key)) {
        return false;
      }
    }
  }
  return true;
}
export function startPendingSync({
  doc,
  onChange,
  remoteOrigin,
  send,
  store,
}: PendingSyncOptions): PendingSync {
  let destroyed = false;
  // Whether the current socket's admission is read-only, from its authentication on.
  let readOnly = false;
  // This tab's local updates that changed only what the editor derives, each with the ordinal it
  // was made at, kept in memory until the server acknowledges one. Counted or stored, one would
  // name an edit nobody made - a read-only admission refuses a heading-id stamp, and the stamp of a
  // reconnect's editor can come before its admission is known. Each is still a struct of this
  // tab's client, though, and every later update of the client continues its clock: a stored row
  // typed after one parks on a reload unless the server or the store holds it. So a person's edit
  // records the held updates first, in order.
  let derived: { ordinal: number; update: Uint8Array }[] = [];
  // Whether the stored rows were last restored for a writable admission (false) or kept for a
  // read-only one (true); undefined before the first. A later admission with the same flag
  // changes nothing the stored rows need.
  let syncedReadOnly: boolean | undefined;
  let rebuiltAt: number | undefined;
  let lastState: PendingState | undefined;
  let nextOrdinal = 0;
  let reporting: Promise<void> | undefined;
  let reportAgain = false;
  let storageAvailable: boolean | undefined;
  const persistedSeqByOrdinal = new Map<number, number>();
  // The newest row this tab has asked the store to hold, by the `seq` the store assigned it: a
  // report whose count was asked for while this grew holds a document without that row, and is
  // dropped rather than given (`report`).
  let recordedThrough = 0;
  let recorded: RecordedUpdate[] = [];
  let sent: SentFrame[] = [];
  let refused: SentFrame | undefined;
  // Whether the rows this browser holds have waited out `UNSENT_NOTICE_MS` without the server
  // taking a frame, and the timer that is counting that wait down.
  let unsent = false;
  let unsentWait: ReturnType<typeof setTimeout> | undefined;
  /** Starts the wait over: the server has taken a frame, or this browser holds no row. */
  const restartUnsentWait = (): void => {
    if (unsentWait !== undefined) {
      clearTimeout(unsentWait);
      unsentWait = undefined;
    }
    unsent = false;
  };

  const ready = store
    .catch((error: unknown) => {
      console.error("Could not open the browser's pending document edits", error);
      return undefined;
    })
    .then((edits) => {
      storageAvailable = edits !== undefined;
      if (!storageAvailable) {
        recorded = [];
        sent = [];
        derived = [];
      }
      return edits;
    });
  const report = (): Promise<void> => {
    if (reporting !== undefined) {
      reportAgain = true;
      return reporting;
    }
    reporting = (async () => {
      do {
        reportAgain = false;
        const edits = await ready;
        if (destroyed) {
          return;
        }
        // Read before the count is asked for, so what the count answers for is what the store
        // had been asked to hold when its transaction was created.
        const countedThrough = recordedThrough;
        const count = edits === undefined ? 0 : await edits.count();
        // A row this tab wrote while the count was in flight is a row the count did not hold:
        // IndexedDB runs transactions whose scopes overlap in the order they were created, so
        // this count reads a document that has yet to hold that row, and on a document with
        // nothing else pending it reads none pending. Whoever waits for this browser's edits to
        // reach the server (`pending-waiter.ts`) would be answered by it while the row it waits
        // for is still being written, so there is no report to give here: the row's own write
        // asks for a fresh one as soon as it settles, whether the store takes the row or
        // refuses it (`record`).
        if (countedThrough !== recordedThrough) continue;
        if (count === 0) {
          // Everything this browser held has reached the server, so nothing is waiting.
          restartUnsentWait();
        } else if (!unsent && unsentWait === undefined) {
          unsentWait = setTimeout(() => {
            unsentWait = undefined;
            unsent = true;
            void report();
          }, UNSENT_NOTICE_MS);
        }
        const state: PendingState = {
          count,
          readOnly,
          rebuiltAt,
          stored: edits !== undefined,
          unsent,
        };
        if (
          lastState === undefined ||
          lastState.count !== state.count ||
          lastState.readOnly !== state.readOnly ||
          lastState.rebuiltAt !== state.rebuiltAt ||
          lastState.stored !== state.stored ||
          lastState.unsent !== state.unsent
        ) {
          lastState = state;
          onChange(state);
        }
      } while (reportAgain);
    })()
      .catch((error: unknown) => {
        console.error("Could not count the browser's pending document edits", error);
      })
      .finally(() => {
        reporting = undefined;
      });
    return reporting;
  };

  const record = (update: Uint8Array, ordinal: number): RecordedUpdate => {
    const entry: RecordedUpdate = {
      ordinal,
      seq: undefined,
      update,
      written: Promise.resolve(),
    };
    recorded.push(entry);
    entry.written = ready
      .then(async (edits) => {
        if (edits === undefined) {
          return;
        }
        // The store counts and reads from the moment it opens, and keys a row only once it knows
        // the client id this tab's rows are keyed by, the document's first (`PendingEdits.keyed`).
        await edits.keyed;
        const row = edits.record(update);
        entry.seq = row.seq;
        recordedThrough = row.seq;
        persistedSeqByOrdinal.set(entry.ordinal, row.seq);
        return row.written;
      })
      .then(
        () => report(),
        (error: unknown) => {
          console.error("Could not store a pending document edit", error);
          // The report this row's write was left to give (`report`, above) is owed whether the
          // store took the row or refused it - the browser's quota, say: the count a report in
          // flight dropped for this row is nowhere else, so without this the toolbar keeps the
          // count it was showing, and a `whenSent` waiting on that report runs to its bound.
          return report();
        }
      );
    return entry;
  };

  // The stored rows of every client but this tab's. This tab's rows are the ones under the id its
  // store keys them by (`PendingEdits.keyed`): the document's first id, which is not always the one
  // it writes under, since a later socket can move a document that has written nothing to another
  // id (`connection.ts`, `takeClientId`). They hold edits this document already has, which this
  // tab's own frames carry and their answers clear.
  const otherClientsRows = async (edits: PendingEdits): Promise<RestoredEdit[]> => {
    const own = await edits.keyed;
    return (await edits.restoreRows()).filter((row) => row.key[1] !== own);
  };

  // The editor's repair is never kept. A page whose schema lacks an element is admitted read-only,
  // and the server drops what that socket sends. A later writable admission would replay a kept
  // repair, and delete for everyone the element this editor could not show.
  const onUpdate = (
    update: Uint8Array,
    origin: unknown,
    _doc: Y.Doc,
    transaction: Y.Transaction
  ) => {
    if (
      destroyed ||
      storageAvailable === false ||
      origin === remoteOrigin ||
      isEditorRepair(origin, transaction)
    ) {
      return;
    }
    // An update that changes only what the editor derives is held in memory, uncounted and
    // unstored, until the server acknowledges it or a person's edit stores it.
    if (changesOnlyDerived(transaction)) {
      nextOrdinal += 1;
      derived.push({ ordinal: nextOrdinal, update });
      return;
    }
    // A read-only admission's editor takes no typing, so any other update it makes is still one
    // the editor derives from the document as loaded, which the server refuses and the next load
    // derives again: no edit of a person's to keep. Should the admission turn writable, the
    // reconnect's SyncStep2, which carries all this document holds that the server lacks, sends it.
    if (readOnly) {
      return;
    }
    // A person's edit continues the clock of the updates held before it, so they are stored first
    // and in order, and its row applies on a reload whether or not the server took them.
    for (const held of derived) {
      record(held.update, held.ordinal);
    }
    derived = [];
    nextOrdinal += 1;
    record(update, nextOrdinal);
  };
  doc.on("update", onUpdate);

  const clearUpdate = async (frame: SentFrame & { kind: "update" }): Promise<void> => {
    const edits = await ready;
    if (edits === undefined) {
      return;
    }
    // A provider Update frame carries exactly the Yjs event bytes. Separate local Yjs updates
    // have distinct client-id/clock ranges, so only a retry of this same row can match these bytes.
    const held = derived.findIndex((entry) => equals(entry.update, frame.update));
    if (held !== -1) {
      derived.splice(held, 1);
      return;
    }
    const index = recorded.findIndex((entry) => equals(entry.update, frame.update));
    if (index === -1) {
      console.error("The document server acknowledged an update this browser did not store.");
      return;
    }
    const [entry] = recorded.splice(index, 1);
    if (entry === undefined) {
      return;
    }
    await entry.written;
    if (entry.seq === undefined) {
      return;
    }
    await edits.clear(entry.seq);
    persistedSeqByOrdinal.delete(entry.ordinal);
    await report();
  };

  const clearThrough = async (through: number): Promise<void> => {
    const edits = await ready;
    if (edits === undefined || through === 0) {
      return;
    }
    derived = derived.filter((entry) => entry.ordinal > through);
    const covered = recorded.filter((entry) => entry.ordinal <= through);
    await Promise.all(covered.map((entry) => entry.written));
    // Filter the list as it is now, not as it was before the await: an edit typed while those
    // writes were pending was appended since, and stays tracked for its own acknowledgement.
    const coveredSet = new Set(covered);
    recorded = recorded.filter((entry) => !coveredSet.has(entry));

    let seq = 0;
    const acknowledged: number[] = [];
    for (const [ordinal, persistedSeq] of persistedSeqByOrdinal) {
      if (ordinal <= through) {
        seq = Math.max(seq, persistedSeq);
        acknowledged.push(ordinal);
      }
    }
    if (seq > 0) {
      await edits.clearThrough(seq);
      for (const ordinal of acknowledged) {
        persistedSeqByOrdinal.delete(ordinal);
      }
    }
    await report();
  };

  void report();

  // Restores the stored rows for the current admission, once its socket has synced: a writable one
  // replays the rows other tabs and earlier pages left, and a read-only one keeps them all for the
  // next writable admission to send. No row is dropped because this document already holds what it
  // carries: the document holds what the room relayed, which the room sends before its store takes
  // it, so holding a row's content says nothing of whether the server keeps it. Each row is sent
  // again, and cleared only by an applied answer that covers it (`clearUpdate`).
  const admit = async (): Promise<void> => {
    const admittedReadOnly = readOnly;
    syncedReadOnly = admittedReadOnly;
    const edits = await ready;
    if (destroyed || edits === undefined || admittedReadOnly) {
      await report();
      return;
    }
    try {
      const restored = await otherClientsRows(edits);
      if (destroyed || restored.length === 0) {
        await report();
        return;
      }
      // Every row is judged and applied in one synchronous pass, so the scratch copy stays the
      // live document's equal: it takes each row the live document takes, and is copied afresh
      // only after a row it refused, whose parked structs would otherwise hide the next stale
      // row from the same lost history. One document encode, plus one per refused row.
      let scratch = scratchOf(doc);
      // The clients whose content marks a row as typed against a history a rebuild replaced: the
      // ones the rebuilt document lists as retired, which the room refuses for good, and the
      // clients of each row this restore drops as such, whose later rows continue its clock.
      const replaced = new Set(retiredClients(doc).keys());
      const handled: PendingKey[] = [];
      const rerecorded: Promise<void>[] = [];
      for (const row of restored) {
        // The admission this restore began for can turn read-only while it runs - the issue
        // closed, and this socket was admitted again read-only. The rows left stay for the next
        // writable admission, which this forgets having settled so that `synced` restores them
        // rather than take the admission as one it has already answered.
        if (readOnly !== admittedReadOnly) {
          syncedReadOnly = undefined;
          break;
        }
        handled.push(row.key);
        const parked = parkedClients(scratch, row.update);
        if ([...parked].some((client) => replaced.has(client))) {
          rebuiltAt = Math.min(rebuiltAt ?? row.at, row.at);
          for (const client of Y.parseUpdateMeta(row.update).from.keys()) {
            replaced.add(client);
          }
          scratch = scratchOf(doc);
          continue;
        }
        // The row is applied as the server's own update, which neither this listener records nor
        // the provider sends, and is recorded again under this tab and sent here instead, whole,
        // whatever of it the document already holds. One that builds on content neither this
        // document nor the server holds yet - another person's edit the room relayed to this
        // browser and then lost before its store, which that person's browser kept and sends again
        // - parks here as it does on the scratch, and on the server, which answers it not applied
        // and holds it until that edit lands; it clears on the SyncStep2 that answers the resync
        // the room sends once it has stored what the row waited on.
        Y.applyUpdate(doc, row.update, remoteOrigin);
        nextOrdinal += 1;
        rerecorded.push(record(row.update, nextOrdinal).written);
        send(row.update);
      }
      // The old rows go only once every re-recorded row is stored, in one IndexedDB write.
      await Promise.all(rerecorded);
      // A destroy here leaves the rows; the next first sync replays them again, which loses nothing.
      if (!destroyed) {
        await edits.drop(handled);
      }
      await report();
    } catch (error) {
      console.error("Could not restore the browser's pending document edits", error);
      await report();
    }
  };

  return {
    socketOpened() {
      if (!destroyed) {
        sent = [];
        refused = undefined;
      }
    },
    frameWritten(frame) {
      if (destroyed || storageAvailable === false) {
        return;
      }
      try {
        const decoder = createDecoder(frame);
        readField(decoder); // the document name
        if (readVarUint(decoder) !== MessageType.Sync) {
          return;
        }
        switch (readVarUint(decoder)) {
          case messageYjsSyncStep2:
            // A SyncStep2 is acknowledged as covering every row recorded so far, so only its
            // bounds are checked; its content is never copied.
            readField(decoder);
            sent.push({ kind: "step2", through: nextOrdinal });
            break;
          case messageYjsUpdate:
            sent.push({ kind: "update", update: readField(decoder) });
            break;
        }
      } catch (error) {
        console.error("Could not parse an outgoing document sync frame", error);
      }
    },
    async frameReceived(frame) {
      if (destroyed) {
        return undefined;
      }
      let applied: boolean | undefined;
      try {
        applied = readSyncStatus(frame);
      } catch (error) {
        console.error("Could not parse a document SyncStatus frame", error);
        return undefined;
      }
      if (applied === undefined) {
        return undefined;
      }
      const entry = sent.shift();
      if (entry === undefined) {
        console.error("The document server acknowledged a frame this browser did not send.");
        return applied;
      }
      if (!applied) {
        refused = entry;
        await Promise.all(recorded.map((record) => record.written));
        return applied;
      }
      // The server took a frame of this browser's, so whatever is still saved here has not been
      // waiting on a silent server: the wait starts over.
      restartUnsentWait();
      try {
        if (entry.kind === "update") {
          await clearUpdate(entry);
        } else {
          await clearThrough(entry.through);
        }
      } catch (error) {
        console.error("Could not clear an acknowledged pending document edit", error);
      }
      return applied;
    },
    async dropRefused(restored) {
      const entry = refused;
      refused = undefined;
      await restored;
      // A refused SyncStep2 carries every row at once and names none of them, so its rows stay:
      // the rebuilt document replays each restored row as an Update of its own, and the one the
      // server refuses then is the one dropped.
      if (destroyed || entry?.kind !== "update") {
        return;
      }
      try {
        await clearUpdate(entry);
      } catch (error) {
        console.error("Could not drop a refused pending document edit", error);
      }
    },
    async dropClientRows(restored, clients) {
      const entry = refused;
      refused = undefined;
      await restored;
      // A refused SyncStep2 names no client of its own here, as it names no row: the rebuilt
      // document replays each restored row as an Update of its own, and the client of the one
      // the server refuses then is the one whose rows go.
      const named = new Set(
        clients ?? (entry?.kind === "update" ? Y.parseUpdateMeta(entry.update).from.keys() : [])
      );
      if (destroyed || named.size === 0) {
        return 0;
      }
      const writesUnder = (update: Uint8Array): boolean =>
        [...Y.parseUpdateMeta(update).from.keys()].some((client) => named.has(client));
      try {
        derived = derived.filter((held) => !writesUnder(held.update));
        const dropping: RecordedUpdate[] = [];
        const kept: RecordedUpdate[] = [];
        for (const entry of recorded) {
          (writesUnder(entry.update) ? dropping : kept).push(entry);
        }
        recorded = kept;
        await Promise.all(dropping.map((entry) => entry.written));
        for (const entry of dropping) {
          persistedSeqByOrdinal.delete(entry.ordinal);
        }
        const edits = await ready;
        if (edits === undefined) {
          return dropping.length;
        }
        const keys = (await edits.restoreRows())
          .filter((row) => writesUnder(row.update))
          .map((row) => row.key);
        if (keys.length > 0) {
          await edits.drop(keys);
        }
        await report();
        return keys.length;
      } catch (error) {
        console.error("Could not drop the pending document edits under a refused client id", error);
        return 0;
      }
    },
    authenticated(nextReadOnly) {
      readOnly = nextReadOnly;
    },
    async synced() {
      if (readOnly !== syncedReadOnly) {
        await admit();
      }
    },
    destroy() {
      if (destroyed) {
        return;
      }
      destroyed = true;
      restartUnsentWait();
      derived = [];
      doc.off("update", onUpdate);
      void ready.then((edits) => edits?.close());
    },
  };
}
