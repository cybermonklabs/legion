import { expect, jest, spyOn, test } from "bun:test";
import { screen, waitFor, within } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";

import { realClock } from "../../__tests__/fake-clock";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError } from "../../api/client";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { renderRecipients, subscriber } from "./ask-recipients-fixture";

// An unfollow or an unsubscribe the reader stopped, and one made again that Dispatch answers
// `404`: what the recipients say of each.

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
        again: "Retry",
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
        again: "Retry",
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
