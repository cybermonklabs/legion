import {
  createContext,
  type ReactNode,
  useCallback,
  useContext,
  useEffect,
  useLayoutEffect,
  useMemo,
  useState,
} from "react";

import { calloutWarningBorder, surfaceBg } from "../../theme/shell-classes";
import { ChunkBoundary } from "../shell/ChunkBoundary";
import { type Chunk, chunk, chunkComponent } from "../shell/chunk-component";
import {
  importWhenOnline,
  keptUntilRejected,
  prefetchChunk,
  renderChunk,
} from "../shell/chunk-loads";
import { useContentDrawn } from "../shell/content-drawn";
import { ErrorView, ReloadButton } from "../shell/ErrorBoundary";
import { KeptWrites } from "./KeptWrites";
import {
  PENDING_WRITES_FRAME,
  PENDING_WRITES_PLACE,
  PendingWritesStatus,
} from "./pending-writes-frame";
import { changesCount, indicatorWrites, keptRecords, keptWrites } from "./shown-writes";
import type { KeptWrite } from "./write-storage";
import {
  kindsRegistered,
  useKindsFailed,
  useNotTakenUp,
  useWriteSelection,
  type Write,
} from "./write-store";

/** No record of what a reload kept: the line's while the kinds of write have not failed. */
const NO_RECORDS: readonly KeptWrite[] = [];

/** No row of the indicator's: the standalone line's when the indicator draws its own rows. */
const NO_WRITES: readonly Write[] = [];

/**
 * Where the shell places its two lists of writes, and where the code of each comes from: the
 * pending-writes indicator in the status corner of every signed-in page
 * (`PendingWritesIndicator.tsx`), and what a lapsed session keeps, on the sign-in page
 * (`KeptWrites.tsx`). Where every kind of write the page makes is fetched too (`kinds.ts`).
 *
 * The indicator draws nothing while it holds no write, which is how almost every page load finds
 * it, and it carried 2,588 bytes of the entry chunk that every page parsed before it drew
 * anything. So it is a chunk of its own, and its host here reads the same selection of the one
 * write store the indicator reads (`shown-writes.ts`): no indicator before the first row. It
 * stays mounted from then on and draws nothing once none is left (`PendingWritesHost` says why).
 *
 * The kept list is in the entry, with no chunk. It is the reader's one warning, before they sign
 * in as someone else, of the writes that sign-in drops, so it draws with the sign-in page,
 * whatever becomes of any download: as a chunk of its own it would draw one download late on a
 * page that loads signed out, and not at all where that download failed online. At `80d018f6`
 * that warning cost every page 1,030 bytes of the entry, the record's Discard
 * (`Writes.discardKept`) included. It is drawn only while it holds a row, of the store's or of
 * what the reload kept that the store has not taken up (`KeptWritesHost`).
 *
 * The indicator is drawn through `chunkComponent` (`features/shell/chunk-component.tsx`), as the
 * issue page's tab panels, a document's decision cards and the reference-preview layer are; a
 * route goes through `lazyChunk`, and a surface a key opens through `askedSurface`. A chunk that
 * is in draws in the commit of the row that shows it, and a download that fails goes through
 * `renderChunk`, so an indicator that cannot load after a deploy spends one of the tab's recovery
 * reloads (`DeploymentResilience.tsx`). Both hosts sit outside the page's error boundary - the
 * status corner, and the sign-in page - so each list has a boundary of its own (`ChunkBoundary`),
 * and the rest of the page stays drawn and signed in. An indicator whose chunk fails to download
 * online draws a line in its place that counts the changes not sent, with a Reload
 * (`ChangesNotSent`); a list whose render throws draws an alert in its place (`CouldNotShow`).
 * A change of which writes a list shows, or of where one stands - a write pressed or gone, one
 * refused or late - draws the list afresh (`reset`, `shownWritesKey`, `keptRecordsKey`), which
 * asks for the indicator's chunk again, and the line stays in the indicator's place while that
 * download is out, also through an outage (`WhileIndicatorLoads`); the indicator's last write
 * going is no such change, since the list it would draw is empty (`PendingWritesHost`). A
 * boundary that never reset would leave the reader on a page no recovery reload follows - the
 * boundary says when none does - with no Retry, Discard or Stop trying for the life of the page.
 * The indicator's chunk is started once the page has drawn, so a reader who presses a write
 * usually finds it in already.
 */
const pendingWritesChunk = chunk<object>(() =>
  import("./PendingWritesIndicator").then((module) => module.PendingWritesIndicator)
);

/**
 * What the indicator draws while its chunk is on its way, which its host hands down
 * (`pendingWritesHost`): nothing, as it would with no write to show, until a download of the
 * chunk has failed online, and from then on the line in its place (`ChangesNotSent`). Each change
 * of `reset` draws the indicator afresh, and it asks for its chunk again as it mounts
 * (`chunkComponent`), which offline waits for the network (`importWhenOnline`). With nothing
 * drawn meanwhile, the line would go at each such change, and stay away for a whole outage, which
 * is when the reader needs it. So it stays through every later ask of the chunk, until the chunk
 * draws the indicator or no write is left.
 */
const WhileIndicatorLoads = createContext<{
  readonly checkLoaded: () => void;
  readonly line: ReactNode;
}>({ checkLoaded: () => {}, line: null });

/** The indicator's loading fallback: what its host hands down (`WhileIndicatorLoads`), telling the
 *  host as it unmounts whether the indicator's chunk has landed (`checkLoaded`). */
function IndicatorLoading(): ReactNode {
  const { checkLoaded, line } = useContext(WhileIndicatorLoads);
  useLayoutEffect(() => () => checkLoaded(), [checkLoaded]);
  return line;
}

/**
 * What stands in a list's place once its render has thrown: an alert that says which list could
 * not be shown, with a Reload, as a region that stopped rendering looks (`ErrorView`). A render
 * error is never silent here: the indicator's rows are the one place a write shown there
 * (`presentation: "indicator"`) offers Retry and Discard once refused, and the sign-in page's
 * list is the reader's one warning of the writes a sign-in as someone else drops. It is no
 * dialog, as a surface's alert is (`askedSurface`): the reader did not open the list, so it takes
 * no focus and holds no keys. Its role is `alert`, as a region's is (`ErrorBoundary`), so a
 * screen reader says it as it appears, wherever the focus is.
 */
function CouldNotShow({ className, list }: { className: string; list: string }): ReactNode {
  const heading = `Could not show ${list}.`;
  return (
    <ErrorView
      aria-label={heading}
      className={className}
      heading={heading}
      reload="Reload"
      role="alert"
    />
  );
}

/**
 * What stands in the indicator's place once its chunk has failed to download online: a line that
 * counts the writes the indicator shows, "1 change not sent", with "Not saved" once Dispatch has
 * refused one, and a Reload, as a region that stopped rendering offers (`ErrorView`). The
 * indicator is the one place where a reader learns that a write is still out or was refused, and
 * where they press Retry or Discard. The leave prompt shows only as the reader leaves the page,
 * and the next write draws the indicator again only for a reader who makes one. So the failure is
 * never silent: the line stays until the chunk draws the indicator, through every later ask of
 * the chunk, online or offline (`WhileIndicatorLoads`). The line is in the entry and reads the
 * host's rows, so it needs no chunk. It draws nothing once no write is left. It stands where the
 * indicator stands, in the indicator's frame and status line (`pending-writes-frame.tsx`), with a
 * border of its own.
 *
 * It counts `records` too: what the reload kept that the store has not taken up, while the
 * take-up waits for kinds of write whose load failed online (`Writes.kindsFailed`). Those are no
 * row of the store's, so neither the indicator nor a composer shows them, and the kinds may never
 * come down with no recovery reload to follow; read from their own records, with no kind of
 * write in, they are counted here in the same words, "Not saved" for one kept as refused.
 */
function ChangesNotSent({
  records,
  rows,
}: {
  records: readonly KeptWrite[];
  rows: readonly Write[];
}): ReactNode {
  const total = rows.length + records.length;
  if (total === 0) return null;
  const count = changesCount(total);
  const refused =
    rows.some((row) => row.status === "refused") ||
    records.some((record) => record.state.status === "refused");
  return (
    <div className={`${PENDING_WRITES_FRAME} ${surfaceBg} ${calloutWarningBorder}`}>
      <PendingWritesStatus>
        {refused ? `${count} not sent — Not saved` : `${count} not sent`}
      </PendingWritesStatus>
      <ReloadButton>Reload</ReloadButton>
    </div>
  );
}

/** The line, as the indicator's boundary draws it once a download of the indicator's chunk has
 *  failed online (`notLoaded`). It tells the host so before paint (`failed`), so the standalone
 *  line gives way without ever painting two boxes, and tells the host as it unmounts whether the
 *  indicator's chunk has landed (`checkLoaded`). */
function NotLoaded({
  checkLoaded,
  failed,
  records,
  rows,
}: {
  checkLoaded: () => void;
  failed: (failed: boolean) => void;
  records: readonly KeptWrite[];
  rows: readonly Write[];
}): ReactNode {
  useLayoutEffect(() => {
    failed(true);
    return () => checkLoaded();
  }, [checkLoaded, failed]);
  return <ChangesNotSent records={records} rows={rows} />;
}

/**
 * Every kind of write the page makes (`kinds.ts`), which only the store's take-up of what a
 * reload kept reads (`Writes.resume`): a write the page makes itself registers its own kind as
 * the module its component imports for it is evaluated. So the kinds are a chunk of their own,
 * about 22,000 bytes no first paint draws taken out of the entry (dispatch://INFRA-1109, R13).
 */
const writeKinds = keptUntilRejected(() => import("./kinds"));

/**
 * Loads every kind of write, for the store's take-up (`app.tsx`, `Writes.resume`), which every
 * write the reader presses meanwhile waits behind. Under `renderChunk`, since a reader waits on
 * it: a download that fails spends one of the tab's recovery reloads (`DeploymentResilience.tsx`),
 * and the reloaded page takes up every write this one held, kept in the tab from its press.
 * Under `importWhenOnline`, so a download that fails offline is asked for again once the network
 * is back, rather than reloading into the browser's offline page.
 */
export function loadWriteKinds(): Promise<unknown> {
  return renderChunk(() => importWhenOnline(writeKinds));
}

/**
 * Starts the kinds of write for a host, nobody waiting on them yet (`prefetchChunk`), and once
 * the load answers ends every take-up still waiting for them (`kindsRegistered`). In the app that
 * end is never the one that counts: every load of the kinds goes through `writeKinds`, which
 * keeps the module once it has come in, so the load that brings them in evaluates `kinds.ts`,
 * whose evaluation ends every take-up waiting then, and a take-up that begins after that ends
 * through its own ask, answered from what is in (`Writes.resume`). It counts only where a
 * take-up's own ask is not `writeKinds` and fails after the kinds are in - the loader a test
 * hands `Writes.resume` (`write-hosts.test.tsx`) - and there it keeps the take-up's end from
 * turning on which load evaluated the module.
 */
function prefetchWriteKinds(): void {
  prefetchChunk(() => writeKinds().then(kindsRegistered));
}

/** What a list's boundary draws afresh on (`ChunkBoundary`'s `reset`): which of the store's
 *  writes it shows, and where each stands. A change of either may be the reader's way to the
 *  list's controls - a write pressed or gone, one refused with Retry and Discard, one late with
 *  Stop trying - while an upload's progress, which changes a row at every percent, changes
 *  neither: a boundary whose chunk keeps failing would otherwise ask for it again at every
 *  percent of an upload. */
function shownWritesKey(rows: readonly Write[]): string {
  return rows.map((row) => `${row.id}:${row.status}`).join(",");
}

/** The same for the kept records the store has not taken up, which only the kept list shows:
 *  which records it shows, and where each stands. */
function keptRecordsKey(records: readonly KeptWrite[]): string {
  return records.map((record) => `${record.key}:${record.state.status}`).join(",");
}

/**
 * The pending-writes indicator's host over `source`, the indicator's chunk: the shell's
 * (`PendingWritesHost`), and, in `write-hosts.test.tsx`, hosts over a chunk of the row's own,
 * whose downloads the row fails, holds and answers as a deploy or an outage does the indicator's.
 * The shell's chunk cannot stand in those rows: once a row has drawn the indicator it is in, and
 * `chunk` keeps it.
 */
export function pendingWritesHost(source: Chunk<object>): () => ReactNode {
  const PendingWritesIndicator = chunkComponent(source, <IndicatorLoading />);
  return function PendingWritesHost(): ReactNode {
    const rows = useWriteSelection("indicator", indicatorWrites);
    const shown = useMemo(() => shownWritesKey(rows), [rows]);
    // What the boundary draws afresh on (`ChunkBoundary`'s `reset`): the last list of writes the
    // indicator showed. An empty list is no change of it, so the last write going leaves it as it
    // was. A boundary that failed would otherwise draw the indicator afresh for a list that draws
    // nothing, and the indicator asks for its chunk as it draws (`chunkComponent`), which can
    // spend one of the tab's recovery reloads (`DeploymentResilience.tsx`) for that empty list;
    // the next write the reader presses asks again. It is empty only until the first row, so it
    // says too whether the indicator is mounted: from the first row on, and never unmounted for
    // want of one. The indicator draws nothing itself while nothing is pending, and the focus it
    // hands on as its panel goes - after a Discard or a Stop trying that takes the last row away -
    // is a layout effect of its own (`usePanelFocusReturn`), which runs only in a commit it is
    // still mounted in. Unmounted in the commit its last row goes, it would leave the focus on the
    // page's body. Set in the render that sees the change, so that render draws it already.
    const [reset, setReset] = useState("");
    if (shown !== "" && shown !== reset) setReset(shown);
    // Whether a download of the indicator's chunk has failed online under this host (`NotLoaded`),
    // and whether that chunk has landed (`source.loaded()`): while it failed and has not landed,
    // the line inside the boundary (`NotLoaded`, `WhileIndicatorLoads`) counts both the
    // indicator's rows and the kept records waiting on failed kinds as one box. Once the chunk
    // lands (or while it has never failed), the indicator draws its own rows - or nothing once
    // they land - and a standalone line draws any kept records waiting on failed kinds beside it,
    // so mounting the indicator never hides them for the life of the page.
    const [chunkFailed, setChunkFailed] = useState(false);
    const [chunkLoaded, setChunkLoaded] = useState(() => source.loaded() !== undefined);
    const checkLoaded = useCallback(() => {
      if (source.loaded() !== undefined) setChunkLoaded(true);
    }, []);
    const indicatorDrawingLine = chunkFailed && !chunkLoaded;
    // What the reload kept that the store has not taken up, while the take-up waits for kinds of
    // write whose load failed online (`Writes.kindsFailed`): read from the records alone, with no
    // kind of write in, and counted on the line, since no other surface of a signed-in page shows
    // them.
    const kindsFailed = useKindsFailed();
    const notTakenUp = useNotTakenUp();
    const waiting = useMemo(
      () => (kindsFailed ? keptRecords(notTakenUp) : NO_RECORDS),
      [kindsFailed, notTakenUp]
    );
    const whileLoading = useMemo(
      () => ({
        checkLoaded,
        line: indicatorDrawingLine ? <ChangesNotSent records={waiting} rows={rows} /> : null,
      }),
      [checkLoaded, indicatorDrawingLine, rows, waiting]
    );
    // The indicator's chunk is started here, after the page has drawn
    // (`features/shell/content-drawn.ts`): an effect runs after the commit it belongs to, so a
    // landing does not ask for it before its first paint, and a landing that draws a live
    // document asks only once its first block is in. Nobody is waiting on it yet, which is what
    // `prefetchChunk` tells the chunk-failure recovery. So are the kinds of write: the take-up
    // asked for them as the person signed in, and this finds that download in or on its way. One
    // the take-up lost before the page drew, with no reload to follow - the page was being left,
    // the reader chose to stay at the leave prompt that the writes the reload kept
    // (`WriteStorage.load`) or a write pressed meanwhile open, or the tab's recovery reloads are
    // spent - is asked for again here, and its evaluation ends the take-up (`kindsRegistered`)
    // with no press to ask for them (`Writes.run`).
    const drawn = useContentDrawn();
    useEffect(() => {
      if (!drawn) return;
      prefetchChunk(source.load);
      prefetchWriteKinds();
    }, [drawn]);
    // A list whose render threw keeps its alert in its place until `reset` changes, and then
    // draws again, so a fault that one write's row brings ends with that row. The alert goes with
    // the last write as well: with no write left, the list has nothing it could not show. A list
    // whose chunk could not load keeps the line that counts its writes in its place until `reset`
    // changes too, and then while the indicator drawn afresh asks for its chunk again, until the
    // chunk draws the indicator. The line goes with the last write: with no write left, no change
    // is out (`ChangesNotSent`).
    if (reset === "") {
      return waiting.length === 0 ? null : <ChangesNotSent records={waiting} rows={rows} />;
    }
    return (
      <>
        {!indicatorDrawingLine && waiting.length > 0 ? (
          <ChangesNotSent records={waiting} rows={NO_WRITES} />
        ) : null}
        <WhileIndicatorLoads value={whileLoading}>
          <ChunkBoundary
            fallback={
              shown === "" ? null : (
                <CouldNotShow
                  className={`${PENDING_WRITES_PLACE} ${surfaceBg}`}
                  list="the changes waiting to save"
                />
              )
            }
            notLoaded={
              <NotLoaded
                checkLoaded={checkLoaded}
                failed={setChunkFailed}
                records={indicatorDrawingLine ? waiting : NO_RECORDS}
                rows={rows}
              />
            }
            reset={reset}
          >
            <PendingWritesIndicator />
          </ChunkBoundary>
        </WhileIndicatorLoads>
      </>
    );
  };
}

/** The pending-writes indicator, for the shell's status corner. */
export const PendingWritesHost = pendingWritesHost(pendingWritesChunk);

/** What a lapsed session keeps, for the sign-in page. */
export function KeptWritesHost(): ReactNode {
  const rows = useWriteSelection("kept-while-signed-out", keptWrites);
  // What the reload kept that the store has not taken up, read from each write's own record with
  // no kind of write in. This list is the reader's one warning, before they sign in as someone
  // else, of the writes that sign-in drops, so it lists these at once, also while the kinds are
  // still on their way; a write the store takes up later shows as the store's row instead.
  const records = keptRecords(useNotTakenUp());
  // The kinds of write, for a take-up of what a reload kept that still waits for them as the
  // session lapses (`Writes.resume`): it takes them up all the same, for the sign-in that
  // follows. The list needs none of them. The shell that asked for them is gone, so if its
  // download failed with no reload to follow, nothing else on this page would ask again. Nobody
  // waits on them here, which is what `prefetchChunk` tells the chunk-failure recovery. A load
  // that fails here too is asked for again as the reader comes back to the page - the window gets
  // the focus back, or the browser comes back online - since the reader presses no write on the
  // sign-in page, and a press is what asks again elsewhere (`Writes.run`).
  useEffect(() => {
    prefetchWriteKinds();
    window.addEventListener("focus", prefetchWriteKinds);
    window.addEventListener("online", prefetchWriteKinds);
    return () => {
      window.removeEventListener("focus", prefetchWriteKinds);
      window.removeEventListener("online", prefetchWriteKinds);
    };
  }, []);
  // A list whose render threw keeps its alert in its place until a change of the writes it shows
  // - the store's rows or the kept records it has not taken up - or of where one stands draws it
  // again, so a fault that one write's row brings ends with that row. The alert goes with the
  // last write as well: once neither `rows` nor `records` holds anything, this host draws
  // nothing and unmounts the boundary. `records` is a new array at each render, so the key is
  // read afresh each time.
  const reset = `${shownWritesKey(rows)}|${keptRecordsKey(records)}`;
  return rows.length === 0 && records.length === 0 ? null : (
    <ChunkBoundary
      fallback={<CouldNotShow className="mt-6" list="the changes this page kept" />}
      reset={reset}
    >
      <KeptWrites records={records} rows={rows} />
    </ChunkBoundary>
  );
}
