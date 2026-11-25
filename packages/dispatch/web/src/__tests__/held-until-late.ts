/**
 * The check that a control's hold ends at its write's deadline (`KeyedWriteResult.late`): a write
 * that never answers - each try fails at the network, and the next waits out its backoff - holds
 * the control it was pressed from until `SEND_DEADLINE_MS`, and no longer.
 */

import { expect } from "bun:test";
import type { QueryClient } from "@tanstack/react-query";
import type { UserEvent } from "@testing-library/user-event";

import type { WriteBounds } from "../api/client";
import { emulateChromiumFocusFixup } from "../features/issue-page/chromium-focus-fixup";
import { SEND_DEADLINE_MS } from "../features/writes/keyed-write";
import { writesOf } from "../features/writes/write-store";
import { tick } from "./fake-clock";
import { heldNotDisabled, pressHeld } from "./held-press";

/** How many presses `writes` were made by: the keys their attempts went out under, each attempt's
 *  bounds being the last of its arguments (`WriteBounds`). A write that fails at the network is
 *  tried again under the same key, so its tries count as the one press. */
export function pressesOf(
  writes: readonly { readonly mock: { readonly calls: readonly (readonly unknown[])[] } }[]
): number {
  return new Set(
    writes.flatMap((write) => write.mock.calls.map((call) => (call.at(-1) as WriteBounds).key))
  ).size;
}

/** Presses `pressed` as `pressHeld` does - `start`, `press` and `sent` are its own - and checks
 *  the hold that follows ends at the write's deadline. A second short of `SEND_DEADLINE_MS` the
 *  write `queryClient`'s store holds under `label` (`Write.label`) is still `sending`, and
 *  `pressed` held, with the focus; past it the write is `late` and still tried, and `pressed` is
 *  let go, with the focus on it. `alongside` checks what the write holds beside `pressed`: it is
 *  called with `true` a second short of the deadline, and with `false` past it. Chromium's focus
 *  fixup is on throughout, so a control that turns disabled drops the focus. The case holds the
 *  fake clock (`jest.useFakeTimers()`), which this moves, and `user` is a user on it
 *  (`userOnFakeClock`); the write is left late, for the case to press again or to put away. */
export async function heldUntilLate(
  user: UserEvent,
  pressed: HTMLElement & { readonly disabled: boolean },
  {
    alongside,
    label,
    name,
    press,
    queryClient,
    sent,
    start,
  }: {
    alongside?: (held: boolean) => void;
    label: string;
    name: string;
    press?: string;
    queryClient: QueryClient;
    sent: () => number;
    start?: HTMLElement;
  }
): Promise<void> {
  const status = () =>
    writesOf(queryClient)
      .rows()
      .find((row) => row.label === label)?.status;
  const stopFocusFixup = emulateChromiumFocusFixup();
  try {
    await pressHeld(user, pressed, { name, press, sent, start });

    await tick(SEND_DEADLINE_MS - 1_000);
    expect(status(), `${label}, a second short of its deadline`).toBe("sending");
    expect(heldNotDisabled(pressed, name), `${name}, a second short of the deadline`).toBe(true);
    expect(pressed, `${name}, a second short of the deadline`).toHaveFocus();
    alongside?.(true);

    await tick(1_000);
    expect(status(), `${label}, past its deadline`).toBe("late");
    expect(heldNotDisabled(pressed, name), `${name}, past the deadline`).toBe(false);
    expect(pressed, `${name}, past the deadline`).toHaveFocus();
    alongside?.(false);
  } finally {
    stopFocusFixup();
  }
}
