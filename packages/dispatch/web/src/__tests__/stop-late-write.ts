import { expect } from "bun:test";
import type { MutationKey, QueryClient } from "@tanstack/react-query";
import { act } from "@testing-library/react";

import { ApiError } from "../api/client";
import type { ReplyTarget } from "../features/conversation/composer-model";
import { heldSends } from "../features/conversation/held-sends";
import type { SentRequest } from "../features/conversation/send-request";
import { SEND_DEADLINE_MS } from "../features/writes/keyed-write";
import { writesOf } from "../features/writes/write-store";
import { onFakeClock, onRealTurns, tick } from "./fake-clock";

/** Waits out the send deadline of the press now in flight and stops the write it left, as the
 *  reader does at the shell's indicator, then waits out the minute the stopped write takes to
 *  settle. The press is the reader's to make again afterwards. The test holds the clock
 *  (`jest.useFakeTimers()`), which `tick` moves. */
export async function stopTheLateWrite(queryClient: QueryClient): Promise<void> {
  await tick(SEND_DEADLINE_MS + 1_000);
  const [late] = writesOf(queryClient).rows();
  expect(late?.status).toBe("late");
  act(() => {
    writesOf(queryClient).stop(late?.id ?? 0);
  });
  await tick(60_000);
}

/**
 * Lets the real event loop turn until `upload` has been called `calls` times: until the press's
 * file is kept and its attempt is out (`onRealTurns`). An upload's attempt, and its send deadline
 * with it, start only once its file is kept (`Writes.#start`), and kept files live in
 * `fake-indexeddb` (`__tests__/setup.ts`), which runs its work on the real `setImmediate`, so
 * `tick` moves the clock and not the keep, and a case that moves the clock past the deadline
 * before the keep is done finds its write still `sending`, with no deadline started. How long the
 * keep takes on the real loop depends on what else is queued there, the previous case's leftovers
 * among them, so a case that does not wait here passes or fails by the order the cases run in.
 */
export async function untilAttempted(
  upload: { readonly mock: { readonly calls: readonly unknown[] } },
  calls: number
): Promise<void> {
  await onRealTurns(() => upload.mock.calls.length >= calls);
}

/** How each held draft is named in a row's title: typed and never sent, refused by Dispatch, or
 *  stopped by the reader. */
export const heldDraftWords = {
  refused: "Dispatch refused",
  stopped: "the reader stopped",
  typed: "never sent",
} as const;

/** A send of `body` on CORE-1 as a comment composer's Send makes it, answering `replyTo`, under
 *  the key `stopped-send`. `fields` names what a host's send has other than that: an anchor, an
 *  edit, another owner or delivery. */
export function sentRequest(
  body: string,
  replyTo: ReplyTarget | null,
  fields: Partial<Omit<SentRequest, "draft" | "replyTo">> = {}
): SentRequest {
  return {
    anchor: undefined,
    ask: { multiple: false, options: [], urgency: "med" },
    draft: { body, mentions: [], replacement: "" },
    edit: undefined,
    kind: "comment",
    owner: { issueKey: "CORE-1", kind: "issue" },
    plainDelivery: "steer",
    replyTo,
    sendKey: "stopped-send",
    ...fields,
  };
}

/** A mocked write of the API's, whose attempts a test answers. */
interface MockedWrite {
  mockRejectedValue(value: unknown): unknown;
}

/** Sends `request` under `mutationKey` through the held-send store, as that composer's Send does,
 *  to a server that is down - each of `writes` answers its attempts `503` - and stops it once it
 *  is late, as the reader's Stop trying does (`stopTheLateWrite`): the write may have been
 *  delivered, and the store holds it stopped under its key. It runs on the fake clock, and gives
 *  the real one back however it ends. */
export async function stopSend(
  queryClient: QueryClient,
  mutationKey: MutationKey,
  request: SentRequest,
  writes: readonly MockedWrite[]
): Promise<void> {
  const down = new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" });
  for (const write of writes) write.mockRejectedValue(down);
  await onFakeClock(async () => {
    act(() => {
      heldSends(queryClient).send(mutationKey, request);
    });
    await stopTheLateWrite(queryClient);
  });
}

/** Sends `body` as a reply to `replyTo` on CORE-1 under `mutationKey`, and stops it once it is
 *  late (`stopSend`), `createComment` answering its attempts. */
export async function stopReplySend(
  queryClient: QueryClient,
  createComment: MockedWrite,
  mutationKey: MutationKey,
  body: string,
  replyTo: ReplyTarget
): Promise<void> {
  await stopSend(queryClient, mutationKey, sentRequest(body, replyTo), [createComment]);
}
