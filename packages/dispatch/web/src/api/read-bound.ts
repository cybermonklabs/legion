import { onlineManager } from "@tanstack/react-query";
import { useCallback, useState } from "react";

import { NETWORK_ERROR_MESSAGE, REQUEST_DEADLINE_MS, requestTimeoutMessage } from "./client";

/**
 * Waits for `read`, the re-read that draws what a landed write changed, for `REQUEST_DEADLINE_MS`
 * at the most. The timer starts at this call, whatever state the query is in. TanStack pauses a
 * fetch the browser starts offline until the network returns, and no request deadline runs while
 * a fetch is paused, so a paused read alone does not settle for as long as the browser is
 * offline. Answers `undefined` once the read settles, either way. At the bound it answers why the
 * read has not settled, in the words of the error a read that failed for that reason ends with:
 * a `NetworkError`'s while the browser is offline (`onlineManager`), and a `RequestTimeoutError`'s
 * otherwise.
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
 * A control's hold through the re-read that draws what its write changed, bounded by
 * `readWithinBound`. `holdThrough` answers once its read settles or the bound runs out, whichever
 * is first, so a write that returns it from `onSuccess` stays pending, and its control held, for
 * that long and no longer. `unsettled` is why the read of the latest hold that ran out has not
 * settled. The page shows it beside the sentence it shows for a read that failed. It goes once
 * that read settles, and a later hold that runs out puts its own reason in its place.
 */
export function useReadBound(): readonly [
  unsettled: string | undefined,
  holdThrough: (read: Promise<unknown>) => Promise<void>,
] {
  const [unsettled, setUnsettled] = useState<{
    readonly read: Promise<unknown>;
    readonly reason: string;
  }>();
  const holdThrough = useCallback(async (read: Promise<unknown>) => {
    const reason = await readWithinBound(read);
    if (reason === undefined) return;
    setUnsettled({ read, reason });
    const settled = () => setUnsettled((current) => (current?.read === read ? undefined : current));
    read.then(settled, settled);
  }, []);
  return [unsettled?.reason, holdThrough];
}
