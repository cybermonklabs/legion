import { expect, jest, spyOn, test } from "bun:test";
import { IDEMPOTENCY_KEY_TTL_MS } from "@legion/contracts";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";

import { jump, realClock } from "../../__tests__/fake-clock";
import { keptTab, keyMinted } from "../../__tests__/kept-sends-harness";
import { answerSends } from "../../__tests__/send-answers";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, type WriteBounds, type WriteMethod } from "../../api/client";
import { emulateChromiumFocusFixup } from "../issue-page/chromium-focus-fixup";
import {
  expiredRetryNote,
  FIRST_BACKOFF_MS,
  SEND_DEADLINE_MS,
  sameKeyRetryNote,
} from "./keyed-write";
import { PendingWritesIndicator } from "./PendingWritesIndicator";
import { KEPT_SEND_RESUME_MS } from "./write-storage";
import { plainWrite, type Writes, writesOf } from "./write-store";

/** One write the indicator shows, labelled `label`, its attempt `write`, which makes a request of
 *  `method`. */
function indicatorWrite(
  writes: Writes,
  label: string,
  method: WriteMethod,
  write: (bounds: WriteBounds) => Promise<unknown>
): void {
  act(() => {
    void writes
      .run({ detail: null, kind: plainWrite, label, method, presentation: "indicator", write })
      .catch(() => undefined);
  });
}

const refusal = new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" });
const verdict = new ApiError(422, { code: "KEY_REUSED", error: "the key names another write" });

/** The shell's indicator over one app's write queue, which the test drives directly: the rows are
 *  the queue's, whichever surface made them. */
function mountIndicator(): { client: QueryClient; unmount: () => void } {
  const client = new QueryClient({ defaultOptions: { mutations: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  const view = render(<PendingWritesIndicator />, { wrapper });
  return { client, unmount: () => view.unmount() };
}

async function settle(): Promise<void> {
  await act(async () => {
    for (let turn = 0; turn < 20; turn += 1) await Promise.resolve();
  });
}

// Nothing pending, nothing on screen: the indicator is chrome, and an empty one would sit over the
// page for ever.
test("the indicator shows nothing while no write is out", () => {
  const { unmount } = mountIndicator();
  try {
    expect(screen.queryByTestId("pending-writes")).not.toBeInTheDocument();
  } finally {
    unmount();
  }
});

// What a write is, how many are out, and what the reader may do about each: the count while they
// are within their deadline, the "not confirmed" line and Stop trying once one is late, and -
// once the reader stops it - its name with Retry and Discard. A write that lands leaves.
test("the indicator counts the writes out, names a late one, and clears when it lands", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const tab = keptTab();
  const patchIssue = spyOn(api, "patchIssue").mockRejectedValue(refusal);
  const held = Promise.withResolvers<never>();
  const putIssueState = spyOn(api, "putIssueState").mockReturnValue(held.promise);
  const { client, unmount } = mountIndicator();
  const writes = writesOf(client);

  try {
    indicatorWrite(writes, "CORE-1 set to done", "PATCH", (bounds) =>
      api.patchIssue("CORE-1", { status: "done" }, bounds)
    );
    indicatorWrite(writes, "CORE-2 pinned", "PUT", (bounds) =>
      api.putIssueState("CORE-2", { pinned: true }, bounds)
    );
    await settle();

    const indicator = await screen.findByTestId("pending-writes");
    expect(within(indicator).getByRole("status").textContent).toBe("Saving 2 changes…");
    expect(
      within(indicator).queryByRole("button", { name: "Stop trying" })
    ).not.toBeInTheDocument();

    await jump(SEND_DEADLINE_MS);
    expect(within(indicator).getByRole("status").textContent).toBe(
      "Dispatch has not confirmed 2 changes — still trying"
    );
    expect(within(indicator).getByText("CORE-1 set to done")).toBeTruthy();
    expect(within(indicator).getByText("CORE-2 pinned")).toBeTruthy();
    expect(within(indicator).getAllByRole("button", { name: "Stop trying" })).toHaveLength(2);

    // Stop trying ends the first one: it names itself with why, and offers Retry and Discard.
    await user.click(
      within(indicator).getAllByRole("button", { name: "Stop trying" })[0] as Element
    );
    await jump(30 * 60_000);
    expect(
      within(indicator).getByText(/^Not saved: CORE-1 set to done — Dispatch has not confirmed it/)
    ).toBeTruthy();
    expect(within(indicator).getByRole("button", { name: "Retry" })).toBeTruthy();
    expect(within(indicator).getByRole("button", { name: "Discard" })).toBeTruthy();
    expect(within(indicator).getByRole("status").textContent).toBe(
      "Dispatch has not confirmed 1 change — still trying"
    );

    // Discard drops the refusal; the pin write lands, and the indicator goes with it.
    await user.click(within(indicator).getByRole("button", { name: "Discard" }));
    await settle();
    expect(screen.queryByText("CORE-1 set to done")).not.toBeInTheDocument();

    // The pin write is stopped too; its attempt in flight then fails, and its Retry lands, so the
    // indicator goes with the last write it held.
    await user.click(within(indicator).getByRole("button", { name: "Stop trying" }));
    await settle();
    held.reject(refusal);
    await jump(30 * 60_000);
    expect(within(indicator).getByRole("button", { name: "Retry" })).toBeTruthy();

    putIssueState.mockResolvedValueOnce({ pinned: true } as never);
    await user.click(within(indicator).getByRole("button", { name: "Retry" }));
    await settle();
    await waitFor(() => expect(screen.queryByTestId("pending-writes")).not.toBeInTheDocument());
  } finally {
    unmount();
    patchIssue.mockRestore();
    putIssueState.mockRestore();
    tab.close();
    realClock();
  }
});

/** The indicator on a page as the shell draws it: after a control the reader comes to it from
 *  (`Elsewhere`, drawn while `elsewhere` holds), before another control and a line of plain text,
 *  and beside the page's main region, which takes the focus as `app.tsx`'s does. */
function ShellPage({ elsewhere }: { readonly elsewhere: boolean }): ReactNode {
  return (
    <>
      {elsewhere ? <button type="button">Elsewhere</button> : null}
      <PendingWritesIndicator />
      <button type="button">Other</button>
      <p>Page text</p>
      <main id="main-content" tabIndex={-1} />
    </>
  );
}

/** Mounts `ShellPage` over one app's write queue; answers the queue, what takes `Elsewhere` off
 *  the page, and the unmount. */
function mountOnPage(): { dropElsewhere: () => void; unmount: () => void; writes: Writes } {
  const client = new QueryClient({ defaultOptions: { mutations: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  const view = render(<ShellPage elsewhere />, { wrapper });
  return {
    dropElsewhere: () => view.rerender(<ShellPage elsewhere={false} />),
    unmount: () => view.unmount(),
    writes: writesOf(client),
  };
}

/** Presses a write whose surface shows its refusal itself (`inline`), as a form does, so the
 *  indicator shows it only while it is out; its one attempt is `attempt`. Waits it late, and
 *  answers the indicator's Stop trying of it. */
async function lateInlineWrite(writes: Writes, attempt: Promise<never>): Promise<HTMLElement> {
  act(() => {
    void writes
      .run({
        detail: null,
        inline: true,
        kind: plainWrite,
        label: "CORE-1 set to done",
        method: "PATCH",
        presentation: "indicator",
        write: () => attempt,
      })
      .catch(() => undefined);
  });
  await settle();
  await jump(SEND_DEADLINE_MS);
  return within(screen.getByTestId("pending-writes")).getByRole("button", { name: "Stop trying" });
}

// Stop trying, pressed by keyboard, keeps the focus while the write's last attempt is out: it
// reads Stopping… and refuses a press (`aria-disabled`) rather than turn disabled, which in
// Chromium would move the focus to the page's body (`emulateChromiumFocusFixup` does that here),
// so a second Enter stops nothing more. Once that attempt fails the write is refused, its own
// surface shows the refusal, and the indicator goes: the focus goes back to the control it came
// from, rather than staying on the page's body.
test("a Stop trying pressed by keyboard keeps the focus while it stops, and gives it back as the indicator goes", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const stopBlurring = emulateChromiumFocusFixup();
  const attempt = Promise.withResolvers<never>();
  const { unmount, writes } = mountOnPage();
  const stops = spyOn(writes, "stop");
  try {
    const stop = await lateInlineWrite(writes, attempt.promise);
    const elsewhere = screen.getByRole("button", { name: "Elsewhere" });
    act(() => elsewhere.focus());
    await user.tab();
    expect(stop).toHaveFocus();
    await user.keyboard("{Enter}");
    await settle();
    expect(stop).toHaveTextContent("Stopping…");
    expect(
      stop,
      "Stopping… does not refuse a press while its write's last attempt is out"
    ).toHaveAttribute("aria-disabled", "true");
    expect(stop, "Stop trying lost the focus while its write's last attempt is out").toHaveFocus();
    await user.keyboard("{Enter}");
    await settle();
    expect(stops, "a second Enter on Stopping… stopped the write again").toHaveBeenCalledTimes(1);
    expect(stop).toHaveFocus();

    attempt.reject(refusal);
    await jump(30 * 60_000);
    expect(screen.queryByTestId("pending-writes")).not.toBeInTheDocument();
    expect(elsewhere).toHaveFocus();
  } finally {
    stops.mockRestore();
    unmount();
    stopBlurring();
    realClock();
  }
});

// Where the control the focus came from has left the page, the focus goes to the page's main
// region, which the shell focuses on a page change too. A Discard of the last write the indicator
// holds takes it away as Stop trying does.
test("a Discard that takes the indicator away gives the focus to the main region once its origin has gone", async () => {
  const user = userEvent.setup();
  const { dropElsewhere, unmount, writes } = mountOnPage();
  try {
    indicatorWrite(writes, "CORE-1 set to done", "PATCH", () => Promise.reject(verdict));
    await settle();
    const indicator = await screen.findByTestId("pending-writes");
    act(() => screen.getByRole("button", { name: "Elsewhere" }).focus());
    await user.tab();
    await user.tab();
    expect(within(indicator).getByRole("button", { name: "Discard" })).toHaveFocus();

    dropElsewhere();
    await user.keyboard("{Enter}");
    await settle();
    expect(screen.queryByTestId("pending-writes")).not.toBeInTheDocument();
    expect(document.getElementById("main-content")).toHaveFocus();
  } finally {
    unmount();
  }
});

// A focus the reader took out of the indicator before it went stays where they took it: on
// another control, or on the page's body once they pressed the page's plain text.
test("a focus the reader took out of the indicator stays where they took it as the indicator goes", async () => {
  for (const away of ["Other", "Page text"] as const) {
    jest.useFakeTimers();
    const user = userOnFakeClock();
    const stopBlurring = emulateChromiumFocusFixup();
    const attempt = Promise.withResolvers<never>();
    const { unmount, writes } = mountOnPage();
    try {
      await lateInlineWrite(writes, attempt.promise);
      act(() => screen.getByRole("button", { name: "Elsewhere" }).focus());
      await user.tab();
      await user.keyboard("{Enter}");
      await settle();
      const there = screen.getByText(away);
      await user.click(there);
      const kept = away === "Other" ? there : document.body;
      expect(kept).toHaveFocus();

      attempt.reject(refusal);
      await jump(30 * 60_000);
      expect(screen.queryByTestId("pending-writes")).not.toBeInTheDocument();
      expect(kept).toHaveFocus();
    } finally {
      unmount();
      stopBlurring();
      realClock();
    }
  }
});

// While the indicator is still drawn after a press, the focus stays in it: in the row of the
// control that went, else on the indicator's first control. A write not marked inline stays once
// it is stopped, so the Stop trying pressed by keyboard gives its focus to that row's Retry, not to
// the first row's Stop trying; that row's Discard then takes the row away, and the focus goes to
// the first control left, the other write's Stop trying.
test("a press that leaves the indicator drawn keeps the focus in it, in the same row first", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const stopBlurring = emulateChromiumFocusFixup();
  const first = Promise.withResolvers<never>();
  const second = Promise.withResolvers<never>();
  const { unmount, writes } = mountOnPage();
  try {
    indicatorWrite(writes, "CORE-1 set to done", "PATCH", () => first.promise);
    indicatorWrite(writes, "CORE-2 set to done", "PATCH", () => second.promise);
    await settle();
    await jump(SEND_DEADLINE_MS);
    const indicator = screen.getByTestId("pending-writes");
    const [firstStop, secondStop] = within(indicator).getAllByRole("button", {
      name: "Stop trying",
    });
    act(() => screen.getByRole("button", { name: "Elsewhere" }).focus());
    await user.tab();
    await user.tab();
    expect(secondStop).toHaveFocus();
    await user.keyboard("{Enter}");
    await settle();
    expect(secondStop).toHaveFocus();

    second.reject(refusal);
    await jump(30 * 60_000);
    const line = within(indicator).getByText(/^Not saved: CORE-2 set to done/);
    const stopped = within(line.closest("li") as HTMLElement);
    expect(stopped.getByRole("button", { name: "Retry" })).toHaveFocus();

    await user.tab();
    expect(stopped.getByRole("button", { name: "Discard" })).toHaveFocus();
    await user.keyboard("{Enter}");
    await settle();
    expect(within(indicator).queryByText(/CORE-2/)).not.toBeInTheDocument();
    expect(firstStop).toHaveFocus();
  } finally {
    unmount();
    stopBlurring();
    realClock();
  }
});

/** Presses two writes Dispatch refuses; answers the indicator and the second write's Discard. */
async function twoRefusedWrites(
  writes: Writes
): Promise<{ discard: HTMLElement; indicator: HTMLElement }> {
  indicatorWrite(writes, "CORE-1 set to done", "PATCH", () => Promise.reject(verdict));
  indicatorWrite(writes, "CORE-2 set to done", "PATCH", () => Promise.reject(verdict));
  await settle();
  const indicator = screen.getByTestId("pending-writes");
  const line = within(indicator).getByText(/^Not saved: CORE-2 set to done/);
  const discard = within(line.closest("li") as HTMLElement).getByRole("button", {
    name: "Discard",
  });
  return { discard, indicator };
}

// A focus a pointer press put in the indicator is not handed on: Chromium focuses a pressed
// button and draws it no focus ring, so moving that focus onto the other row's control would put
// it, unseen, where the next Space or Enter presses that control. A Discard pressed by pointer
// that takes its row away leaves the focus where the browser does.
test("a Discard pressed by pointer that takes its row away moves the focus to no other row's control", async () => {
  const user = userEvent.setup();
  const { unmount, writes } = mountOnPage();
  try {
    const { discard, indicator } = await twoRefusedWrites(writes);

    await user.click(discard);
    await settle();
    expect(within(indicator).queryByText(/CORE-2/)).not.toBeInTheDocument();
    expect(document.body).toHaveFocus();
  } finally {
    unmount();
  }
});

/** Taps `target` as a touch screen does, on the fake clock: the `pointerdown` as the finger
 *  lands; then, once it lifts - a tap is held longer than a timer of no delay waits - the
 *  `pointerup`, and only after it the `mousedown` that moves the focus, onto the button tapped or
 *  else off any control, and the `click`. */
async function tap(target: HTMLElement): Promise<void> {
  act(() => {
    target.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true, pointerType: "touch" }));
  });
  await jump(100);
  act(() => {
    target.dispatchEvent(new PointerEvent("pointerup", { bubbles: true, pointerType: "touch" }));
    target.dispatchEvent(new MouseEvent("mousedown", { bubbles: true }));
    const button = target.closest("button");
    if (button !== null) button.focus();
    else if (document.activeElement instanceof HTMLElement) document.activeElement.blur();
    target.dispatchEvent(new MouseEvent("mouseup", { bubbles: true }));
    target.dispatchEvent(new MouseEvent("click", { bubbles: true }));
  });
  await jump(0);
}

// On a touch screen the browser moves the focus of a tap only as the finger lifts, after the
// tap's `pointerup` (`tap`), well after its `pointerdown`. That focus is the pointer's all the
// same, which the indicator does not hand on: a Discard tapped that takes its row away moves the
// focus to no other row's control.
test("a Discard tapped on a touch screen that takes its row away moves the focus to no other row's control", async () => {
  jest.useFakeTimers();
  const { unmount, writes } = mountOnPage();
  try {
    const { discard, indicator } = await twoRefusedWrites(writes);

    await tap(discard);
    await settle();
    expect(within(indicator).queryByText(/CORE-2/)).not.toBeInTheDocument();
    expect(document.body).toHaveFocus();
  } finally {
    unmount();
    realClock();
  }
});

// Where a tap elsewhere left the focus is read once the tap is done, as the finger lifts on a
// touch screen (`tap`): a keyboard reader who stopped a write and then tapped the page's plain
// text, which leaves the focus on the page's body, keeps it there as the indicator goes.
test("a tap on the page's plain text keeps the focus off the indicator's origin as the indicator goes", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const stopBlurring = emulateChromiumFocusFixup();
  const attempt = Promise.withResolvers<never>();
  const { unmount, writes } = mountOnPage();
  try {
    const stop = await lateInlineWrite(writes, attempt.promise);
    act(() => screen.getByRole("button", { name: "Elsewhere" }).focus());
    await user.tab();
    await user.keyboard("{Enter}");
    await settle();
    expect(stop).toHaveFocus();
    await tap(screen.getByText("Page text"));
    expect(document.body).toHaveFocus();

    attempt.reject(refusal);
    await jump(30 * 60_000);
    expect(screen.queryByTestId("pending-writes")).not.toBeInTheDocument();
    expect(document.body).toHaveFocus();
  } finally {
    unmount();
    stopBlurring();
    realClock();
  }
});

// A key ends a pointer press no `click` ended, as when the pointer is dragged off the control
// before it is let go: the focus the keyboard then moves into the indicator is noted, so the Stop
// trying pressed by keyboard gives the focus back as the indicator goes.
test("a key after a pointer press no click ended lets the indicator note where the keyboard takes the focus", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const stopBlurring = emulateChromiumFocusFixup();
  const attempt = Promise.withResolvers<never>();
  const { unmount, writes } = mountOnPage();
  try {
    const stop = await lateInlineWrite(writes, attempt.promise);
    act(() => {
      stop.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true }));
    });
    const elsewhere = screen.getByRole("button", { name: "Elsewhere" });
    act(() => elsewhere.focus());
    await user.tab();
    expect(stop).toHaveFocus();
    await user.keyboard("{Enter}");
    await settle();

    attempt.reject(refusal);
    await jump(30 * 60_000);
    expect(screen.queryByTestId("pending-writes")).not.toBeInTheDocument();
    expect(elsewhere).toHaveFocus();
  } finally {
    unmount();
    stopBlurring();
    realClock();
  }
});

// A touch that scrolls the indicator's list ends in a `pointercancel` and no `click`, which ends
// the press as a key does: the focus a Stop trying pressed by keyboard hands to its row's Retry,
// once its write is refused, is noted as the focus coming in, so the Discard the reader then
// presses gives the focus back to where it came from as the indicator goes.
test("a touch press on the indicator that the browser cancels lets it note the focus it hands on", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const stopBlurring = emulateChromiumFocusFixup();
  const attempt = Promise.withResolvers<never>();
  const { unmount, writes } = mountOnPage();
  try {
    indicatorWrite(writes, "CORE-1 set to done", "PATCH", () => attempt.promise);
    await settle();
    await jump(SEND_DEADLINE_MS);
    const indicator = screen.getByTestId("pending-writes");
    const elsewhere = screen.getByRole("button", { name: "Elsewhere" });
    act(() => elsewhere.focus());
    await user.tab();
    expect(within(indicator).getByRole("button", { name: "Stop trying" })).toHaveFocus();
    await user.keyboard("{Enter}");
    await settle();
    const list = within(indicator).getByRole("list");
    act(() => {
      for (const type of ["pointerdown", "pointercancel"]) {
        list.dispatchEvent(new PointerEvent(type, { bubbles: true, pointerType: "touch" }));
      }
    });

    attempt.reject(refusal);
    await jump(30 * 60_000);
    expect(within(indicator).getByRole("button", { name: "Retry" })).toHaveFocus();
    await user.tab();
    expect(within(indicator).getByRole("button", { name: "Discard" })).toHaveFocus();
    await user.keyboard("{Enter}");
    await settle();
    expect(screen.queryByTestId("pending-writes")).not.toBeInTheDocument();
    expect(
      elsewhere,
      "the focus Retry took from the Stop trying it replaced was not noted as coming from Elsewhere"
    ).toHaveFocus();
  } finally {
    unmount();
    stopBlurring();
    realClock();
  }
});

// A press elsewhere that leaves the focus in the indicator - on a control whose `mousedown` is
// prevented, as a toolbar's button keeps the focus where it was - forgets nothing: the Stop
// trying the reader then presses by keyboard gives the focus back as the indicator goes.
test("a press elsewhere that leaves the focus in the indicator keeps where the focus came from", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const stopBlurring = emulateChromiumFocusFixup();
  const attempt = Promise.withResolvers<never>();
  const { unmount, writes } = mountOnPage();
  const other = screen.getByRole("button", { name: "Other" });
  const keepFocus = (event: MouseEvent) => event.preventDefault();
  other.addEventListener("mousedown", keepFocus);
  try {
    const stop = await lateInlineWrite(writes, attempt.promise);
    const elsewhere = screen.getByRole("button", { name: "Elsewhere" });
    act(() => elsewhere.focus());
    await user.tab();
    await user.click(other);
    expect(stop).toHaveFocus();
    await user.keyboard("{Enter}");
    await settle();
    expect(stop).toHaveFocus();

    attempt.reject(refusal);
    await jump(30 * 60_000);
    expect(screen.queryByTestId("pending-writes")).not.toBeInTheDocument();
    expect(elsewhere).toHaveFocus();
  } finally {
    other.removeEventListener("mousedown", keepFocus);
    unmount();
    stopBlurring();
    realClock();
  }
});

// A write Dispatch refused says so in its own words, and its Retry asks again - under a key of
// its own, since Dispatch replays the refusal it stored under the first.
test("a refused write names Dispatch's reason, and Retry sends it again", async () => {
  const user = userEvent.setup();
  const tab = keptTab();
  const patchIssue = spyOn(api, "patchIssue").mockRejectedValueOnce(verdict);
  const { client, unmount } = mountIndicator();
  const writes = writesOf(client);

  try {
    indicatorWrite(writes, "CORE-1 set to done", "PATCH", (bounds) =>
      api.patchIssue("CORE-1", { status: "done" }, bounds)
    );
    await settle();

    const indicator = await screen.findByTestId("pending-writes");
    expect(
      within(indicator).getByText("Not saved: CORE-1 set to done — the key names another write")
    ).toBeTruthy();
    expect(within(indicator).queryByRole("status")).not.toBeInTheDocument();

    patchIssue.mockResolvedValueOnce({ key: "CORE-1" } as never);
    await user.click(within(indicator).getByRole("button", { name: "Retry" }));
    await settle();
    expect(patchIssue).toHaveBeenCalledTimes(2);
    expect(patchIssue.mock.calls[1]?.[2]?.key).not.toBe(patchIssue.mock.calls[0]?.[2]?.key);
    await waitFor(() => expect(screen.queryByTestId("pending-writes")).not.toBeInTheDocument());
  } finally {
    unmount();
    patchIssue.mockRestore();
    tab.close();
  }
});

const patch = { body: { status: "done" }, method: "PATCH", path: "/api/v1/issues/CORE-1" } as const;

// W37. A write waiting its turn behind another on its line was never sent: it reads "Waiting to
// send", never "Saving" or "Sending", and offers Discard, which drops it with nothing sent.
test("a write waiting its turn reads Waiting to send and can be discarded", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const { client, unmount } = mountIndicator();
  const writes = writesOf(client);
  const sends = answerSends();
  let secondSent = 0;
  try {
    const press = (label: string, answer: () => Promise<unknown>) =>
      act(() => {
        void writes
          .run({
            detail: null,
            kind: plainWrite,
            label,
            presentation: "indicator",
            request: sends.answer({ ...patch }, answer),
            target: "issue:CORE-1",
          })
          .catch(() => undefined);
      });
    press("CORE-1 set to backlog", () => Promise.withResolvers<never>().promise);
    press("CORE-1 set to done", async () => {
      secondSent += 1;
      return {};
    });
    await settle();
    const indicator = screen.getByTestId("pending-writes");
    expect(within(indicator).getByRole("status").textContent).toBe("Saving 1 change…");
    expect(indicator).toHaveTextContent("CORE-1 set to done — Waiting to send");
    await user.click(within(indicator).getByRole("button", { name: "Discard" }));
    await settle();
    expect(indicator.textContent).not.toContain("Waiting to send");
    expect(writes.rows().map((row) => row.label)).toEqual(["CORE-1 set to backlog"]);
    expect(secondSent).toBe(0);
  } finally {
    sends.restore();
    unmount();
    realClock();
  }
});

// W40. An upload whose body is still going up says how far it has got, and is never "still trying"
// past its deadline until an attempt of it has actually failed; then it is.
test("an upload in progress reads Uploading… N% until an attempt fails", async () => {
  jest.useFakeTimers();
  const { client, unmount } = mountIndicator();
  const writes = writesOf(client);
  const first = Promise.withResolvers<never>();
  let attempts = 0;
  try {
    indicatorWrite(writes, "Upload of spec.md", "POST", (bounds) => {
      attempts += 1;
      bounds.sent?.({
        body: { name: "spec.md" },
        method: "POST",
        path: "/api/v1/issues/CORE-1/artifacts",
      });
      if (attempts > 1) return Promise.withResolvers<never>().promise;
      bounds.progress?.(40, 100);
      return first.promise;
    });
    await settle();
    await jump(SEND_DEADLINE_MS + 1_000);
    const indicator = screen.getByTestId("pending-writes");
    expect(within(indicator).getByRole("status").textContent).toBe("Saving 1 change…");
    expect(indicator).toHaveTextContent("Upload of spec.md — Uploading… 40%");
    expect(indicator.textContent).not.toContain("still trying");

    first.reject(refusal);
    await settle();
    expect(within(indicator).getByRole("status").textContent).toBe(
      "Dispatch has not confirmed 1 change — still trying"
    );
    expect(indicator.textContent).not.toContain("Uploading…");
  } finally {
    unmount();
    realClock();
  }
});

// A write whose key Dispatch has forgotten is never asked again under it. The key matters only
// once something has gone out under it - this attempt tells its request as `api.patchIssue`
// does, and then ends without Dispatch's verdict - so the attempt that would follow is the one
// the spent key stops. The indicator says the key ran out and what a Retry of it would do -
// save the change a second time if the first one was saved - and offers Retry and Discard, as
// it does for any refusal.
test("a write whose key has run out says a Retry saves it again, with Retry and Discard", async () => {
  const spent = keyMinted(Date.now() - IDEMPOTENCY_KEY_TTL_MS);
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const { client, unmount } = mountIndicator();
  const writes = writesOf(client);
  let attempts = 0;

  try {
    act(() => {
      void writes
        .run({
          detail: null,
          key: spent,
          kind: plainWrite,
          label: "CORE-1 set to done",
          method: "PATCH",
          presentation: "indicator",
          write: (bounds) => {
            attempts += 1;
            bounds.sent?.({
              body: { status: "done" },
              method: "PATCH",
              path: "/api/v1/issues/CORE-1",
            });
            return Promise.reject(refusal);
          },
        })
        .catch(() => undefined);
    });
    await settle();
    await jump(FIRST_BACKOFF_MS);
    await settle();

    const indicator = screen.getByTestId("pending-writes");
    expect(
      within(indicator).getByText(
        `Not saved: CORE-1 set to done — Dispatch has not confirmed it. ${expiredRetryNote("saved")}`
      )
    ).toBeTruthy();
    expect(attempts).toBe(1);
    expect(within(indicator).getByRole("button", { name: "Retry" })).toBeTruthy();

    await user.click(within(indicator).getByRole("button", { name: "Discard" }));
    await settle();
    expect(screen.queryByTestId("pending-writes")).toBeNull();
  } finally {
    unmount();
    realClock();
  }
});

/** Presses a write of `method` labelled `label` whose every attempt Dispatch answers `503`, waits
 *  it late and stops it; answers the indicator, which then shows it stopped, and its unmount. */
async function stoppedIn(method: WriteMethod, label: string) {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const { client, unmount } = mountIndicator();
  indicatorWrite(writesOf(client), label, method, (bounds) => {
    bounds.sent?.({ method, path: "/api/v1/issues/CORE-1" });
    return Promise.reject(refusal);
  });
  await settle();
  await jump(SEND_DEADLINE_MS);
  const indicator = screen.getByTestId("pending-writes");
  await user.click(within(indicator).getByRole("button", { name: "Stop trying" }));
  await jump(30 * 60_000);
  return { indicator, unmount };
}

// A `PUT` or a `DELETE` sends no key (`goesUnderKey`), and the store knows that of each write
// from its press (`Write.keyed`): a stopped one says its Retry sends the same change again, not
// that it asks again under the same key.
test("a stopped write that sends no key says its Retry sends the same change again", async () => {
  const { indicator, unmount } = await stoppedIn("DELETE", "Source removed");
  try {
    expect(
      within(indicator).getByText(
        "Not saved: Source removed — Dispatch has not confirmed it. A Retry sends the same change again, which is the same however often it is made."
      )
    ).toBeTruthy();
  } finally {
    unmount();
    realClock();
  }
});

test("a stopped write that goes under its key says its Retry asks again under that key", async () => {
  const { indicator, unmount } = await stoppedIn("PATCH", "CORE-1 set to done");
  try {
    expect(
      within(indicator).getByText(
        `Not saved: CORE-1 set to done — Dispatch has not confirmed it. ${sameKeyRetryNote("saved")}`
      )
    ).toBeTruthy();
  } finally {
    unmount();
    realClock();
  }
});

// A write a reload interrupted too long ago comes back refused, for the reader to Retry: a `PUT`
// kept from its press as one that sends no key (`KeptWrite.keyed`) says its Retry sends the same
// change again, not that it asks again under the same key.
test("a write a reload interrupted says what its Retry does by whether it sends a key", async () => {
  const key = "kept-pin";
  window.sessionStorage.setItem(
    `dispatch.write:${key}`,
    JSON.stringify({
      carried: "PUT",
      detail: null,
      key,
      keyed: false,
      kind: plainWrite.name,
      label: "CORE-2 pinned",
      login: "alice",
      presentation: "indicator",
      pressedAt: Date.now() - KEPT_SEND_RESUME_MS - 1,
      request: { body: { pinned: true }, method: "PUT", path: "/api/v1/me/issues/CORE-2/state" },
      sent: true,
      state: { status: "sending" },
      target: null,
      version: 1,
    })
  );
  const { client, unmount } = mountIndicator();
  try {
    act(() => writesOf(client).resume("alice"));
    expect(
      await screen.findByText(
        "Not saved: CORE-2 pinned — this page was closed before Dispatch confirmed it. A Retry sends the same change again, which is the same however often it is made."
      )
    ).toBeTruthy();
  } finally {
    unmount();
    window.sessionStorage.removeItem(`dispatch.write:${key}`);
  }
});
