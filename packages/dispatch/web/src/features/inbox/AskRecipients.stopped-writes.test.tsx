import { beforeAll, expect, jest, mock, spyOn, test } from "bun:test";
import { screen, waitFor, within } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";

import { realClock, tick } from "../../__tests__/fake-clock";
import { heldUntilLate, pressesOf } from "../../__tests__/held-until-late";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError, type WriteBounds } from "../../api/client";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { renderRecipients, subscriber } from "./ask-recipients-fixture";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

// An unfollow or an unsubscribe the reader stopped, and one made again that Dispatch answers
// `404`: what the recipients say of each, and what a stopped one's Retry is of once the card
// moves to another ask.

/** Presses the first follower's Unfollow and the dialog's Confirm, the Confirm on the held clock
 *  when `held`. */
async function unfollowFirst(user: UserEvent, held: boolean) {
  const section = screen.getByRole("region", { name: "Recipients" });
  await user.click(within(section).getAllByRole("button", { name: "Unfollow" })[0] as HTMLElement);
  const confirm = within(screen.getByRole("dialog")).getByRole("button", { name: "Confirm" });
  if (held) jest.useFakeTimers();
  await (held ? userOnFakeClock() : user).click(confirm);
}

/** Presses Platform PO's Unsubscribe and the dialog's Confirm, the Confirm on the held clock
 *  when `held`. */
async function unsubscribePlatformPo(user: UserEvent, held: boolean) {
  const section = screen.getByRole("region", { name: "Recipients" });
  const viaIssue = await within(section).findByRole("list", { name: "Via issue subscription" });
  await user.click(within(viaIssue).getByRole("button", { name: "Unsubscribe" }));
  const dialog = screen.getByRole("dialog", { name: "Unsubscribe" });
  if (held) jest.useFakeTimers();
  await (held ? userOnFakeClock() : user).click(
    within(dialog).getByRole("button", { name: "Confirm" })
  );
}

test.each([
  "Unfollow",
  "Unsubscribe",
] as const)("%s lets go at the recipients write deadline and sends again", async (name) => {
  const dropped = new NetworkError("/api/v1/asks/ask-1", new TypeError("Failed to fetch"));
  const removeFollower = mock((_ask: string, _session: string, _bounds: WriteBounds) =>
    Promise.reject<void>(dropped)
  );
  const unsubscribe = spyOn(api, "unsubscribeIssueSession").mockRejectedValue(dropped);
  const page = await renderRecipients({
    removeFollower,
    subscribers: [subscriber("b0b0b0b0b0b0b0b0", "Platform PO")],
  });
  try {
    const list = screen.getByRole("list", {
      name: name === "Unfollow" ? "Followers" : "Via issue subscription",
    });
    const button = within(list).getAllByRole<HTMLButtonElement>("button", {
      name,
    })[0] as HTMLButtonElement;
    await page.user.click(button);
    const confirm = within(screen.getByRole("dialog")).getByRole("button", { name: "Confirm" });
    jest.useFakeTimers();
    const user = userOnFakeClock();
    const sent = () => pressesOf([removeFollower, unsubscribe]);
    await heldUntilLate(user, button, {
      label:
        name === "Unfollow"
          ? "session:01234567… taken off this ask"
          : "Platform PO unsubscribed from CORE-1",
      name,
      queryClient: page.queryClient,
      sent,
      start: confirm,
    });
    await user.keyboard("{Enter}");
    await user.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Confirm" }));
    await tick(1_000);
    expect(sent()).toBe(2);
  } finally {
    realClock();
    page.unmount();
    page.queryClient.clear();
    unsubscribe.mockRestore();
  }
});

// An unfollow or an unsubscribe the reader stopped may have been made: the card says so, and
// that its Retry sends the same `DELETE` again, which is the same however often it is made.
test("AskRecipients says an unfollow the reader stopped may have been made, not that it failed", async () => {
  const page = await renderRecipients({
    removeFollower: () =>
      Promise.reject(new NetworkError("/api/v1/asks/ask-1", new TypeError("Failed to fetch"))),
  });
  try {
    await unfollowFirst(page.user, true);
    await stopTheLateWrite(page.queryClient);
    // The alert's paragraph, without its Retry button's name.
    expect(screen.getByRole("alert").querySelector("p")?.textContent).toBe(
      stoppedWriteNote("the unfollow", "made", {
        method: "DELETE",
        stopped: new SendStoppedError(),
      })
    );
  } finally {
    realClock();
    page.unmount();
    page.queryClient.clear();
  }
});

test("AskRecipients says an unsubscribe the reader stopped may have been made, not that it failed", async () => {
  const unsubscribeIssueSession = spyOn(api, "unsubscribeIssueSession").mockRejectedValue(
    new NetworkError("/api/v1/issues/CORE-1/subscribers", new TypeError("Failed to fetch"))
  );
  const page = await renderRecipients({
    subscribers: [subscriber("b0b0b0b0b0b0b0b0", "Platform PO")],
  });
  try {
    await unsubscribePlatformPo(page.user, true);
    await stopTheLateWrite(page.queryClient);
    expect(screen.getByRole("alert").querySelector("p")?.textContent).toBe(
      stoppedWriteNote("the unsubscribe", "made", {
        method: "DELETE",
        stopped: new SendStoppedError(),
      })
    );
  } finally {
    realClock();
    page.unmount();
    page.queryClient.clear();
    unsubscribeIssueSession.mockRestore();
  }
});

// A `DELETE` sends no key, so one made again after an unfollow or an unsubscribe that landed
// finds the session off the ask already and is answered `404`: the change done, which the card
// takes as it takes a `204`, reading again what it changed, not as a refusal.
test("AskRecipients takes an unfollow answered FOLLOWER_NOT_FOUND as the unfollow done", async () => {
  const page = await renderRecipients({
    removeFollower: () =>
      Promise.reject(new ApiError(404, { code: "FOLLOWER_NOT_FOUND", error: "not a follower" })),
  });
  const invalidated: unknown[] = [];
  const original = page.queryClient.invalidateQueries.bind(page.queryClient);
  page.queryClient.invalidateQueries = (filters, ...rest) => {
    invalidated.push(filters?.queryKey);
    return original(filters, ...rest);
  };
  try {
    await unfollowFirst(page.user, false);
    await waitFor(() => expect(invalidated).toContainEqual(["ask-thread", "ask-1"]));
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  } finally {
    page.unmount();
  }
});

test("AskRecipients takes an unsubscribe answered SUBSCRIBER_NOT_FOUND as the unsubscribe done", async () => {
  const unsubscribeIssueSession = spyOn(api, "unsubscribeIssueSession").mockRejectedValue(
    new ApiError(404, { code: "SUBSCRIBER_NOT_FOUND", error: "session is not subscribed" })
  );
  const page = await renderRecipients({
    subscribers: [subscriber("b0b0b0b0b0b0b0b0", "Platform PO")],
  });
  const invalidated: unknown[] = [];
  const original = page.queryClient.invalidateQueries.bind(page.queryClient);
  page.queryClient.invalidateQueries = (filters, ...rest) => {
    invalidated.push(filters?.queryKey);
    return original(filters, ...rest);
  };
  try {
    await unsubscribePlatformPo(page.user, false);
    await waitFor(() => expect(invalidated).toContainEqual(["subscribers", "CORE-1"]));
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  } finally {
    page.unmount();
    unsubscribeIssueSession.mockRestore();
  }
});

/** Presses the Retry of the card's one refusal, on the clock the case holds. */
async function retryTheRefusal() {
  await userOnFakeClock().click(
    within(screen.getByRole("alert")).getByRole("button", { name: "Retry" })
  );
  await tick(1_000);
}

// A host draws one card for one ask after another, and a stopped unfollow's refusal stays on the
// card as it moves. Its Retry is still that unfollow, so it takes the session off the ask it was
// pressed on, not off the one the card shows by then.
test("AskRecipients retries a stopped unfollow on the ask it was pressed on after the card moves to another ask", async () => {
  const removeFollower = mock((_askId: string, _sessionId: string, _bounds: WriteBounds) =>
    Promise.reject<void>(new NetworkError("/api/v1/asks/ask-1", new TypeError("Failed to fetch")))
  );
  const page = await renderRecipients({ removeFollower });
  try {
    await unfollowFirst(page.user, true);
    await stopTheLateWrite(page.queryClient);
    const stoppedKey = removeFollower.mock.calls[0]?.[2].key;
    const stopped = removeFollower.mock.calls.length;

    page.rerender({ askId: "ask-2" });
    removeFollower.mockImplementation(async () => {});
    await retryTheRefusal();

    expect(removeFollower).toHaveBeenCalledTimes(stopped + 1);
    const again = removeFollower.mock.calls.at(-1);
    expect(again?.[0]).toBe("ask-1");
    expect(again?.[1]).toBe("0123456789abcdef");
    expect(again?.[2].key).toBe(stoppedKey);
  } finally {
    realClock();
    page.unmount();
    page.queryClient.clear();
  }
});

// So does an unsubscribe: its Retry takes the session off the subscription of the ask it was
// pressed on, the first issue's, not off that of the issue whose ask the card shows by then.
test("AskRecipients retries a stopped unsubscribe on the issue it was pressed on after the card moves to another issue's ask", async () => {
  const unsubscribeIssueSession = spyOn(api, "unsubscribeIssueSession").mockRejectedValue(
    new NetworkError("/api/v1/issues/CORE-1/subscribers", new TypeError("Failed to fetch"))
  );
  const page = await renderRecipients({
    subscribers: [subscriber("b0b0b0b0b0b0b0b0", "Platform PO")],
  });
  try {
    await unsubscribePlatformPo(page.user, true);
    await stopTheLateWrite(page.queryClient);
    const stoppedKey = unsubscribeIssueSession.mock.calls[0]?.[2].key;
    const stopped = unsubscribeIssueSession.mock.calls.length;

    page.rerender({ askId: "ask-2", owner: { artifact_id: null, issue_key: "CORE-2" } });
    unsubscribeIssueSession.mockResolvedValue(undefined);
    await retryTheRefusal();

    expect(unsubscribeIssueSession).toHaveBeenCalledTimes(stopped + 1);
    const again = unsubscribeIssueSession.mock.calls.at(-1);
    expect(again?.[0]).toBe("CORE-1");
    expect(again?.[1]).toBe("b0b0b0b0b0b0b0b0");
    expect(again?.[2].key).toBe(stoppedKey);
  } finally {
    realClock();
    page.unmount();
    page.queryClient.clear();
    unsubscribeIssueSession.mockRestore();
  }
});
