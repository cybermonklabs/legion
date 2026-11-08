import { createContext, useContext, useRef, useSyncExternalStore } from "react";

/**
 * Whether the page is still waiting for the editor's first batch, and the one answer every read
 * no reader is waiting for is held behind (dispatch://INFRA-341, R11).
 *
 * A document address spends its first seconds building one thing: the block a reader can type
 * in. Measured on this pod, `iphone` at 4x CPU throttling, the issue page made seven reads on
 * that path that the block needs none of - the issue's comments and asks, the agent registry,
 * the repository mappings, the project's architecture sources, the issue's subscribers and the
 * spec's `part_of` references - each costing the main thread the request, the answer's parse
 * and a panel's draw while the reader waited. They are the issue page's non-document panels,
 * and they are read after the editor's first batch instead.
 *
 * Module state rather than a React value, because the panels that read it are not under one
 * component: the margin lives above the router (`app.tsx`'s `MarginColumn`), the header lives
 * inside the issue page, and the answer has to be there before either renders. The address
 * opens it (`open-document-early.ts`, which runs before React has rendered anything), and the
 * document surface closes it once that first batch's blocks are in the editor, in the task
 * they landed in (`ProofDocument`'s `onBatch`).
 *
 * Nothing is ever held for a page with no document on it: `awaited` is false until an address
 * that opens one says otherwise, so every other page reads exactly as it did.
 *
 * Nor is a read held from a reader who has reached its panel. `useAfterTheFirstBatch` takes
 * that as its argument - the Subscribers panel opened, the repository picker reached for, the
 * review sheet pulled up - and `ReaderIsHere` says the same for a whole subtree, so an agent
 * name inside a panel the reader opened reads the registry then rather than at the batch.
 */
let awaited = false;
const listeners = new Set<() => void>();

/**
 * How long the page holds those reads for a batch that has not come.
 *
 * Every way a first batch arrives has an end that says so, and each of them releases the hold:
 * the batch itself (`ProofDocument`'s `onBatch`), an issue or a text read that names no
 * document to open (`open-document-early.ts`), a transport or editor chunk that failed and a
 * stored tree the server will not admit (`ProofDocument`'s `reportLoadFailure` and its blocked
 * admission). What has no end of its own is a load that stops in the middle: a chunk that
 * never arrives, a socket that neither connects nor fails, a server that takes the page's
 * SyncStep1 and sends nothing back. Nothing reports any of those, and the page's header and
 * tab strip stay a skeleton with nothing left to release them (`AfterTheFirstBatch.tsx`'s
 * `ChromeAfterTheFirstBatch`). This is the deadline over the whole of that load;
 * `LONGEST_WAIT_FOR_THE_DOCUMENT_MS` below is the shorter one over its last step, where the
 * page is waiting on nothing but a round trip.
 *
 * So the hold is a bargain with a deadline rather than an open one. The reader has the
 * document's own text on the screen about 0.6 s into the load whatever the socket does
 * (`first-screen.ts`), so what the hold costs them when it runs long is not the document but
 * the page around it - the header, the tab strip, the artifact's chrome, the way out of a
 * document that is not opening - and no reader should lose that to a connection that is never
 * going to answer.
 *
 * Twenty seconds. The deadline exists for a page that is never going to be told anything, so
 * what it must not do is fire on a page that is merely slow, and the two are told apart by
 * nothing but the clock. A load this branch measures reaches its first editable block at
 * 6.8 s on a 28-section document and 8.2 s on a 140-section one, `iphone` at 4x CPU
 * throttling on this pod (dispatch://INFRA-341, R11), and the first batch is earlier than
 * either; twenty seconds leaves a slower machine room above that and still ends a wait that
 * would otherwise last as long as the reader stays on the page. Firing costs a slow page the
 * reads' head start and nothing else: the panels read, the chrome lands, and
 * `keepWhatStandsUnder` holds the reader's words where they are (`readers-place.ts`).
 */
export const LONGEST_HOLD_MS = 20_000;

/**
 * How long the page holds them once the socket is open and the server has not answered with
 * the document.
 *
 * The deadline above covers the whole load, main thread included, which is why it is as long
 * as it is: on a throttled phone the socket is not even opened until about six seconds in
 * (`e2e/large-documents-page.ts`, the opening timeline), and the build after it is the page's
 * own thread. This one covers one step of that load, and the step it covers is a round trip:
 * the page has asked the server for the document over an open socket and is waiting for the
 * frame that carries it (`ProofDocument`'s `admit`, `onSynced`). Nothing of the page's own
 * work stands between the two, so a wait that long is a server that is not going to answer -
 * which is the state a held transport leaves a page in, and the one case the deadline above
 * cannot tell from a slow build.
 *
 * Ten seconds: twenty times the half-second R11 calls a freeze, and long enough that a phone
 * on a slow link still gets the hold it was measured for. What firing costs is what firing
 * above costs - the reads' head start, with the reader's place kept under the chrome that
 * lands (`readers-place.ts`) - and what it buys is the way out of a document the server is
 * not sending.
 */
export const LONGEST_WAIT_FOR_THE_DOCUMENT_MS = 10_000;

/** The deadlines armed with the hold above, each cleared by whatever releases it first. */
let deadline: number | undefined;
let serverDeadline: number | undefined;

/**
 * Whether the page waits for a first batch before making the reads above.
 *
 * `true` from the address alone, before anything has rendered: an address that opens a document
 * (`openDocumentEarly`). `false` as soon as nothing is waited for - the first batch is in, the
 * address opens no document, the issue names no document to open, the surface reports that
 * no editor is coming, or either deadline above has passed - and false is where it stays until
 * the next address.
 */
export function theFirstBatchIsAwaited(next: boolean): void {
  window.clearTimeout(deadline);
  window.clearTimeout(serverDeadline);
  serverDeadline = undefined;
  deadline = next
    ? window.setTimeout(() => theFirstBatchIsAwaited(false), LONGEST_HOLD_MS)
    : undefined;
  if (awaited === next) {
    return;
  }
  awaited = next;
  for (const notify of listeners) {
    notify();
  }
}

/**
 * Whether the page is waiting on the server for the document itself: `true` when the document's
 * socket has been opened (`ProofDocument`'s `admit`), `false` when its state has been applied
 * and the editor is being built from it (`onSynced`), which is the step the batch comes out of.
 *
 * Said only by the surface whose document the page is (`page`), as every other report to this
 * gate is: a pane's document holds nothing of the page behind it.
 */
export function theDocumentIsAwaitedFromTheServer(next: boolean): void {
  window.clearTimeout(serverDeadline);
  serverDeadline =
    next && awaited
      ? window.setTimeout(() => theFirstBatchIsAwaited(false), LONGEST_WAIT_FOR_THE_DOCUMENT_MS)
      : undefined;
}

/** Whether the page is past the editor's first batch, for a reader outside React. */
export function afterTheFirstBatch(): boolean {
  return !awaited;
}

/** Tells `listen` whenever the answer above changes; returns the way to stop. */
export function subscribeToTheFirstBatch(listen: () => void): () => void {
  listeners.add(listen);
  return () => {
    listeners.delete(listen);
  };
}

/** Set over a panel the reader has opened: every read inside it is theirs to wait for, so none
 *  of them waits for the batch. Default `false`, which is what every surface outside such a
 *  panel reads. */
export const ReaderIsHere = createContext(false);

/**
 * Whether this read may go out: the first batch is in, this page has none to wait for, or the
 * reader has reached the panel the read fills - `reached`, or a `ReaderIsHere` above it.
 *
 * Once true it stays true for this component, so a reader who opens a panel and closes it again
 * before the batch never has a read taken back from them.
 */
export function useAfterTheFirstBatch(reached = false): boolean {
  const readerIsHere = useContext(ReaderIsHere);
  const landed = useSyncExternalStore(
    subscribeToTheFirstBatch,
    afterTheFirstBatch,
    afterTheFirstBatch
  );
  const allowed = useRef(false);
  allowed.current ||= landed || reached || readerIsHere;
  return allowed.current;
}
