import { expect, setSystemTime } from "bun:test";
import type { QueryClient, QueryKey } from "@tanstack/react-query";
import { act, waitFor } from "@testing-library/react";

import { liveSocketFor } from "../api/live-socket";
import { MIN_REFETCH_INTERVAL_MS } from "../api/query-refresh";
import type { DispatchEvent } from "../api/types";
import { tick } from "./fake-clock";
import { type FakeLiveSocket, installFakeLiveSockets } from "./live-socket-harness";

/** When the corrections below were made, and who deleted a post: alice, as a delete's event
 *  carries its two marks in place of the post's body. */
export const EDITED_AT = "2026-11-20T00:30:00Z";
export const DELETION = {
  deleted_at: "2026-11-20T01:00:00Z",
  deleted_by: { id: "alice", kind: "user" },
} as const;

/** The event id and sequence of the last correction built, so each one follows the last. */
let lastEvent = 0;

/** The edit (`edited`) or the delete of the `kind` post `id` on CORE-1, as the feed carries it,
 *  with `payload` beside the post's id and the correction's own marks: the edit's time, or the
 *  delete's two marks. */
export function correction(
  kind: "comment" | "message",
  id: string,
  edited: boolean,
  payload: Record<string, unknown> = {}
): DispatchEvent {
  lastEvent += 1;
  // The feed's union is wider than a correction; this is the shape its reader reads.
  return {
    actor: DELETION.deleted_by,
    artifact_id: null,
    created_at: edited ? EDITED_AT : DELETION.deleted_at,
    id: lastEvent,
    issue_key: "CORE-1",
    notify: true,
    payload: {
      id,
      issue_key: "CORE-1",
      ...payload,
      ...(edited ? { edited_at: EDITED_AT } : DELETION),
    },
    project: "CORE",
    seq: lastEvent,
    type: `${kind}.${edited ? "edited" : "deleted"}`,
  } as unknown as DispatchEvent;
}

/** The page's event feed on a fake live socket, as `correctionFeed` drives it. */
export interface CorrectionFeed {
  /** Opens the socket the feed asked for and starts its events subscription with nothing to
   *  replay; called once the page has rendered and read what it shows. */
  start(): void;
  /**
   * Delivers `event` on the subscription as Dispatch sends it, a second after anything read so
   * far (`MIN_REFETCH_INTERVAL_MS`), so the scheduler holds back none of the reads it sends, and
   * waits until every read the page draws under each of `failing` (a key prefix) has failed once
   * more than it had, with nothing left fetching: the row's own stub refuses those refetches.
   */
  deliver(event: DispatchEvent, failing: readonly QueryKey[]): Promise<void>;
  /** Puts back the feed's socket, the real `WebSocket` and the clock. */
  restore(): void;
}

/**
 * The page's event feed (`useEventFeed`) on a fake live socket, for a row that delivers a post's
 * correction through the feed and reads what a read of the post keeps once the refetch the event
 * sends fails (dispatch://INFRA-1523, R2 and R3). Made before the page renders, since the feed
 * opens its socket on mount. `fakeClock` is for a row on Bun's fake clock, which the delivery
 * moves on by `tick`; every other row moves the system time.
 */
export function correctionFeed(
  client: QueryClient,
  { fakeClock = false }: { fakeClock?: boolean } = {}
): CorrectionFeed {
  const sockets = installFakeLiveSockets();
  let subscription: { socket: FakeLiveSocket; sub: string } | undefined;
  // The refetches the event sends are of the reads the page draws (`refreshQueries`).
  const failures = (prefixes: readonly QueryKey[]) =>
    prefixes.flatMap((queryKey) =>
      client
        .getQueryCache()
        .findAll({ queryKey })
        .filter((query) => query.isActive())
        .map((query) => query.state.errorUpdateCount)
    );
  return {
    start() {
      const socket = sockets.latest();
      act(() => socket.open());
      const { sub } = socket.subscribed("events");
      act(() => socket.receive({ type: "events_start", sub, since: "0", replay_complete: true }));
      subscription = { socket, sub };
    },
    async deliver(event, failing) {
      if (subscription === undefined) throw new Error("the feed was not started");
      const { socket, sub } = subscription;
      const before = failures(failing);
      expect(before.length, "the reads the delivery should fail are held").toBeGreaterThan(0);
      if (!fakeClock) setSystemTime(Date.now() + MIN_REFETCH_INTERVAL_MS);
      act(() => socket.receive({ type: "event", sub, id: String(event.id), event }));
      // Past the batch's window and the scheduler's interval behind it, which the fake clock
      // runs only as it is moved.
      if (fakeClock) await tick(2 * MIN_REFETCH_INTERVAL_MS);
      await waitFor(() => expect(failures(failing)).toEqual(before.map((count) => count + 1)));
      await waitFor(() => expect(client.isFetching()).toBe(0));
    },
    restore() {
      liveSocketFor(client).close();
      sockets.restore();
      if (!fakeClock) setSystemTime();
    },
  };
}
