import { expect } from "bun:test";
import type { QueryClient } from "@tanstack/react-query";
import { act } from "@testing-library/react";

import { SEND_DEADLINE_MS } from "../features/writes/keyed-write";
import { writesOf } from "../features/writes/write-store";
import { tick } from "./fake-clock";

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
