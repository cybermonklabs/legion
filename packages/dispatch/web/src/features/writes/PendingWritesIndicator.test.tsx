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
import {
  expiredRetryNote,
  FIRST_BACKOFF_MS,
  SEND_DEADLINE_MS,
  sameKeyRetryNote,
} from "./keyed-write";
import { PendingWritesIndicator } from "./PendingWritesIndicator";
import { KEPT_SEND_RESUME_MS } from "./write-storage";
import { defineWriteKind, plainWrite, type Writes, writesOf } from "./write-store";

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

/** A write that carries the reader's own text, which the line offers to copy when a conflict
 *  refuses it: a priority's title, as far as the line is concerned. */
const draftedWrite = defineWriteKind<{ readonly text: string }>({
  draft: (write) => write.detail.text,
  kept: (value): value is { readonly text: string } =>
    typeof value === "object" && value !== null && typeof Reflect.get(value, "text") === "string",
  name: "test-line-draft",
});

/** Has `writes` take up a write a reload kept refused by Dispatch's `status` and `code`, as the
 *  app does once signed in: the write is of `kind`, with `detail` its record. */
function resumedRefusal(
  writes: Writes,
  write: { kind: string; detail: unknown; label: string },
  refusal: { status: number; code: string; message: string }
): void {
  const key = keyMinted(Date.now());
  sessionStorage.setItem(
    `dispatch.write:${key}`,
    JSON.stringify({
      ...write,
      carried: "PATCH",
      key,
      login: "alice",
      presentation: "indicator",
      pressedAt: Date.now(),
      request: { body: { advances: [], advances_revision: 41 }, method: "PATCH", path: "/x" },
      sent: true,
      state: { refusal: { ...refusal, kind: "api" }, status: "refused" },
      target: null,
      version: 1,
    })
  );
  act(() => writes.resume("alice"));
}

// A refusal only the reader can settle - another person changed what the write goes over - is
// refused the same way however often its body is sent: the line says so in its own words and
// offers no Retry, only the reader's own text, drawn as written, to copy, and Discard.
test("a conflict reads why, offers the reader's text to copy and Discard, and no Retry", async () => {
  const user = userEvent.setup();
  const writeText = jest.fn(async (_text: string) => undefined);
  const clipboard = navigator.clipboard;
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
  const { client, unmount } = mountIndicator();
  const writes = writesOf(client);
  const text = "My title,\n  on two lines";
  try {
    act(() => {
      void writes
        .run({
          detail: { text },
          kind: draftedWrite,
          label: "Rename the priority “P1”",
          method: "PATCH",
          presentation: "indicator",
          write: () =>
            Promise.reject(
              new ApiError(409, { code: "PRIORITY_CHANGED", error: "the title was written" })
            ),
        })
        .catch(() => undefined);
    });
    await settle();
    const indicator = await screen.findByTestId("pending-writes");
    expect(
      within(indicator).getByText(
        "Not saved because another person changed it: Rename the priority “P1”"
      )
    ).toBeTruthy();
    expect(within(indicator).queryByText(/^Not saved:/)).toBeNull();
    expect(within(indicator).getByRole("note", { name: "Your text:" }).textContent).toBe(text);
    expect(within(indicator).queryByRole("button", { name: "Retry" })).toBeNull();

    await user.click(within(indicator).getByRole("button", { name: "Copy your text" }));
    expect(writeText).toHaveBeenCalledWith(text);
    expect(await within(indicator).findByRole("button", { name: "Copied" })).toBeTruthy();

    await user.click(within(indicator).getByRole("button", { name: "Discard" }));
    await settle();
    expect(screen.queryByTestId("pending-writes")).toBeNull();
  } finally {
    unmount();
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: clipboard });
  }
});

// A conflict a page mounted now settles with the reader is that page's: the line draws nothing
// of it while the page holds it, and draws it again, with the reader's text to copy and no
// Retry, once the page has gone and handed it back.
test("a conflict a page holds is not on the line, and is again once the page hands it back", async () => {
  const { client, unmount } = mountIndicator();
  const writes = writesOf(client);
  try {
    resumedRefusal(
      writes,
      { detail: { text: "My title" }, kind: draftedWrite.name, label: "Rename the priority “P1”" },
      { code: "PRIORITY_CHANGED", message: "the title was written", status: 409 }
    );
    await settle();
    const id = writes.rows()[0]?.id ?? 0;
    expect(await screen.findByTestId("pending-writes")).toBeTruthy();
    act(() => {
      expect(writes.claim(id)).toBe(true);
    });
    expect(screen.queryByTestId("pending-writes")).toBeNull();
    expect(writes.rows()).toMatchObject([{ claimed: true, status: "refused" }]);

    act(() => writes.unclaim(id));
    const indicator = await screen.findByTestId("pending-writes");
    expect(
      within(indicator).getByText(
        "Not saved because another person changed it: Rename the priority “P1”"
      )
    ).toBeTruthy();
    expect(within(indicator).getByRole("note", { name: "Your text:" }).textContent).toBe(
      "My title"
    );
    expect(within(indicator).queryByRole("button", { name: "Retry" })).toBeNull();
  } finally {
    unmount();
  }
});

// The issue page's Advances picker shows its refusal itself, but a write a reload took up is
// the line's: refused 409 ADVANCES_REVISION, it says another person changed it, with no Retry
// that would send the stale revision again, and no text to copy, since it carried none.
test("a resumed Advances refusal reads as a conflict, with no Retry and nothing to copy", async () => {
  const { client, unmount } = mountIndicator();
  const writes = writesOf(client);
  try {
    resumedRefusal(
      writes,
      { detail: null, kind: plainWrite.name, label: "Advances of CORE-9" },
      { code: "ADVANCES_REVISION", message: "advances revision 41 is stale", status: 409 }
    );
    await settle();
    const indicator = await screen.findByTestId("pending-writes");
    expect(
      within(indicator).getByText("Not saved because another person changed it: Advances of CORE-9")
    ).toBeTruthy();
    expect(within(indicator).queryByRole("button", { name: "Retry" })).toBeNull();
    expect(within(indicator).queryByRole("button", { name: "Copy your text" })).toBeNull();
    expect(within(indicator).getByRole("button", { name: "Discard" })).toBeTruthy();
  } finally {
    unmount();
  }
});

// A new priority whose id one already carries is refused the same way for the same title: the
// line says why, and offers the title to copy rather than a Retry.
test("a new priority whose id is taken reads why, with its title to copy and no Retry", async () => {
  const { client, unmount } = mountIndicator();
  const writes = writesOf(client);
  try {
    resumedRefusal(
      writes,
      { detail: { text: "Evals now" }, kind: draftedWrite.name, label: "New priority “Evals now”" },
      { code: "PRIORITY_EXISTS", message: "a priority evals-now is written", status: 409 }
    );
    await settle();
    const indicator = await screen.findByTestId("pending-writes");
    expect(
      within(indicator).getByText(
        "Not saved because a priority with that id already exists: New priority “Evals now”"
      )
    ).toBeTruthy();
    expect(within(indicator).getByRole("note", { name: "Your text:" }).textContent).toBe(
      "Evals now"
    );
    expect(within(indicator).queryByRole("button", { name: "Retry" })).toBeNull();
  } finally {
    unmount();
  }
});

// Any other refusal of a write carrying the reader's text reads as every refusal does, with
// Retry and Discard: a 500 says nothing of what the write goes over.
test("a 500 refusal still reads Not saved with its reason, Retry and Discard", async () => {
  const { client, unmount } = mountIndicator();
  const writes = writesOf(client);
  try {
    resumedRefusal(
      writes,
      { detail: { text: "Mine" }, kind: draftedWrite.name, label: "Rename the priority “P1”" },
      { code: "INTERNAL", message: "the server failed", status: 500 }
    );
    await settle();
    const indicator = await screen.findByTestId("pending-writes");
    expect(
      within(indicator).getByText("Not saved: Rename the priority “P1” — the server failed")
    ).toBeTruthy();
    expect(within(indicator).getByRole("button", { name: "Retry" })).toBeTruthy();
    expect(within(indicator).getByRole("button", { name: "Discard" })).toBeTruthy();
    expect(within(indicator).queryByRole("button", { name: "Copy your text" })).toBeNull();
  } finally {
    unmount();
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
