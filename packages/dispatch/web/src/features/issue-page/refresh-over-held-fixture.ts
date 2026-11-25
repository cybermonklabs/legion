import { expect } from "bun:test";
import type { QueryClient, QueryKey } from "@tanstack/react-query";
import { act, screen } from "@testing-library/react";

import { CONTROL_LABELS } from "./page-words";

/** The read `failRefreshOverHeld` refreshes over the rows it holds, how it fails, and what the
 *  case checks of it. */
export interface RefreshOverHeld {
  readonly client: QueryClient;
  /** The read's key, whose state the helper asserts is an error once the refresh has failed, and
   *  which it then takes back to holding nothing. */
  readonly queryKey: QueryKey;
  /** Makes the read's next fetch fail. The helper calls it before the refresh and again before
   *  the first read. Left out where the case has made every fetch of the read fail already. */
  readonly failNextFetch?: () => void;
  /** Sends the refresh. */
  readonly refresh: () => Promise<unknown>;
  /** Asserts how many times the read has been read once the refresh has failed, before anything
   *  drawn is read. Left out where the case checks nothing of the refresh beyond its failure. */
  readonly refreshFailed?: () => void;
  /** What the rows hold, in the words the failure message names it by, as `the list`. */
  readonly held: string;
  /** Asserts that the rows the read held are still drawn. */
  readonly stillDrawn: () => void;
}

/**
 * Fails a refresh of a read over the rows it holds, and asserts that the read's state is then an
 * error, that the refresh said no failure and offered no Retry over them, and that the rows stay
 * drawn (`stillDrawn`). Then it takes the read back to holding nothing, as before its first read,
 * and fails that read too, which draws its line and its Retry.
 */
export async function failRefreshOverHeld(read: RefreshOverHeld): Promise<void> {
  read.failNextFetch?.();
  await act(async () => {
    await read.refresh();
  });
  expect(
    read.client.getQueryState(read.queryKey)?.status,
    `the refresh over ${read.held} did not fail`
  ).toBe("error");
  read.refreshFailed?.();
  expect(
    screen.queryByRole("alert"),
    `a failed refresh said a failure over ${read.held}`
  ).toBeNull();
  expect(screen.queryByRole("button", { name: CONTROL_LABELS.retry })).toBeNull();
  read.stillDrawn();
  read.failNextFetch?.();
  await act(async () => {
    await read.client.resetQueries({ queryKey: read.queryKey });
  });
}
