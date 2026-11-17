import { expect, type Mock } from "bun:test";
import type { MutationKey, QueryClient } from "@tanstack/react-query";
import { act } from "@testing-library/react";

import { ApiError, type api } from "../api/client";
import type { ReplyTarget } from "../features/conversation/composer-model";
import { heldSends } from "../features/conversation/held-sends";
import type { SentRequest } from "../features/conversation/send-request";
import { SEND_DEADLINE_MS } from "../features/writes/keyed-write";
import { writesOf } from "../features/writes/write-store";
import { onFakeClock, tick } from "./fake-clock";

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

/** How each held draft is named in a row's title: typed and never sent, refused by Dispatch, or
 *  stopped by the reader. */
export const heldDraftWords = {
  refused: "Dispatch refused",
  stopped: "the reader stopped",
  typed: "never sent",
} as const;

/** Sends `body` as a reply to `replyTo` on CORE-1 under `mutationKey` through the held-send
 *  store, as that composer's Send does, to a server that is down, and stops it once it is late,
 *  as the reader's Stop trying does (`stopTheLateWrite`): the write may have been delivered, and
 *  the store holds it stopped under its key, `stopped-send`. `createComment` answers its
 *  attempts. It runs on the fake clock, and gives the real one back however it ends. */
export async function stopReplySend(
  queryClient: QueryClient,
  createComment: Mock<typeof api.createComment>,
  mutationKey: MutationKey,
  body: string,
  replyTo: ReplyTarget
): Promise<void> {
  createComment.mockRejectedValue(
    new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" })
  );
  const request: SentRequest = {
    anchor: undefined,
    ask: { multiple: false, options: [], urgency: "med" },
    draft: { body, mentions: [], replacement: "" },
    edit: undefined,
    kind: "comment",
    owner: { issueKey: "CORE-1", kind: "issue" },
    plainDelivery: "steer",
    replyTo,
    sendKey: "stopped-send",
  };
  await onFakeClock(async () => {
    act(() => {
      heldSends(queryClient).send(mutationKey, request);
    });
    await stopTheLateWrite(queryClient);
  });
}
