import { onlineManager } from "@tanstack/react-query";
import { useCallback, useRef, useState } from "react";

import { holdHandOff } from "../components/focus-place";
import { NETWORK_ERROR_MESSAGE, REQUEST_DEADLINE_MS, requestTimeoutMessage } from "./client";

/**
 * Waits for `read`, the re-read that draws what a landed write changed, for `REQUEST_DEADLINE_MS`
 * at the most. The timer starts at this call, whatever state the query is in. TanStack pauses a
 * fetch the browser starts offline until the network returns, and no request deadline runs while
 * a fetch is paused, so a paused read alone does not settle for as long as the browser is
 * offline. A read of a query whose fetch the network had already paused is the one that does
 * settle at once, since TanStack answers its refetch there and then: a caller that waits on such
 * a read reads again through `query-client.ts`'s `waitingForPausedReads`, which has it answered
 * when the fetch is, and this bound ends the wait either way. Answers `undefined` once the read
 * settles, either way. At the bound it answers why the read has not settled, in the words of the
 * error a read that failed for that reason ends with: a `NetworkError`'s while the browser is
 * offline (`onlineManager`), and a `RequestTimeoutError`'s otherwise.
 */
export function readWithinBound(read: Promise<unknown>): Promise<string | undefined> {
  const { promise, resolve } = Promise.withResolvers<string | undefined>();
  const bound = setTimeout(
    () =>
      resolve(
        onlineManager.isOnline()
          ? requestTimeoutMessage(REQUEST_DEADLINE_MS)
          : NETWORK_ERROR_MESSAGE
      ),
    REQUEST_DEADLINE_MS
  );
  const settled = () => {
    clearTimeout(bound);
    resolve(undefined);
  };
  read.then(settled, settled);
  return promise;
}

/**
 * Why the latest read handed to `waitWithinBound` has not settled, and nothing of an earlier one.
 * `waitWithinBound` takes a read as it begins, forgets at once why any earlier read had not
 * settled, and answers once the read settles or `readWithinBound`'s bound ends, whichever is
 * first. Where the bound ends first and no later read has begun, it keeps why the read has not
 * settled, which goes once that read settles or a later read begins, whichever is first. An
 * earlier read whose bound ends, or which settles, once a later read has begun changes nothing
 * (`latest`). The page shows `unsettled` beside the sentence it shows for a read that failed.
 */
export function useUnsettledRead(): readonly [
  unsettled: string | undefined,
  waitWithinBound: (read: Promise<unknown>) => Promise<void>,
] {
  // The read of the latest call, the one whose reason the page may show.
  const latest = useRef<Promise<unknown>>(undefined);
  const [unsettled, setUnsettled] = useState<string>();
  const waitWithinBound = useCallback(async (read: Promise<unknown>) => {
    latest.current = read;
    setUnsettled(undefined);
    const reason = await readWithinBound(read);
    if (reason === undefined || latest.current !== read) return;
    setUnsettled(reason);
    const settled = () => {
      if (latest.current === read) setUnsettled(undefined);
    };
    read.then(settled, settled);
  }, []);
  return [unsettled, waitWithinBound];
}

/**
 * A control's hold through the re-read that draws what its write changed, bounded by
 * `readWithinBound`. `holdThrough` answers once its read settles or the bound runs out, whichever
 * is first, so a write that returns it from `onSuccess` stays pending, and its control held, for
 * that long and no longer. While it waits, the hold keeps the armed hand-off of the reader's
 * press that made the write live (`holdHandOff`), so a row or a form that read takes away is still
 * the press's doing (`causedByReader`). `unsettled` is why the read of the latest hold has not
 * settled, once that hold ran out: it goes once that read settles, and at once as a later hold
 * begins, and an earlier hold's read never puts its reason back (`useUnsettledRead`).
 */
export function useReadBound(): readonly [
  unsettled: string | undefined,
  holdThrough: (read: Promise<unknown>) => Promise<void>,
] {
  const [unsettled, waitWithinBound] = useUnsettledRead();
  const holdThrough = useCallback(
    async (read: Promise<unknown>) => {
      const answered = holdHandOff();
      await waitWithinBound(read);
      answered();
    },
    [waitWithinBound]
  );
  return [unsettled, holdThrough];
}
