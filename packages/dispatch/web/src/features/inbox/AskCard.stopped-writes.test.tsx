import { expect, jest, spyOn, test } from "bun:test";
import { act, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { realClock } from "../../__tests__/fake-clock";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { api, NetworkError } from "../../api/client";
import type { Ask, AskRead } from "../../api/types";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { AskCard } from "./AskCard";
import {
  answeredAsk,
  firstAnswer,
  installAskCardApiMocks,
  renderCard,
  thread,
} from "./ask-card-fixture";

// A write the reader stopped on the card: a hand-on, an answer, a question back and a reply may
// each have been made, so the card says what its Retry does rather than that the write failed.

installAskCardApiMocks();

// A hand-on is a `PUT` of whom the ask waits on. One the reader stopped may have been made, so
// the card neither says it failed nor shows the stopped write's own words, which are a
// composer's and speak of a message: it says what its Retry does with the hand-on.
test("a hand-on the reader stopped says what Retry does, not a message's words", async () => {
  const open = answeredAsk({ answer: null, state: "open" });
  const handAskTo = spyOn(api, "handAskTo").mockRejectedValue(
    new NetworkError("/api/v1/asks/ask-1/asked-of", new TypeError("Failed to fetch"))
  );
  const listUsers = spyOn(api, "listUsers").mockResolvedValue([
    { login: "alice" },
    { login: "bob" },
  ]);
  // The hand-on reads the ask's thread again once it settles, stopped or not.
  const getAsk = spyOn(api, "getAsk").mockResolvedValue(thread(open, []));
  const { queryClient, view } = renderCard(<AskCard ask={open} initialThread={thread(open, [])} />);
  const card = within(view.container);
  try {
    const control = await card.findByLabelText<HTMLSelectElement>("Hand this question to");
    // The people are read only once the reader reaches for the control (`AskHandOn`).
    await userEvent.setup().click(control);
    await waitFor(() => expect(within(control).getByRole("option", { name: "bob" })).toBeTruthy());
    jest.useFakeTimers();
    await userOnFakeClock().selectOptions(control, "bob");
    await stopTheLateWrite(queryClient);

    // The alert's paragraph, without its Retry button's name.
    expect(card.getByRole("alert").querySelector("p")?.textContent).toBe(
      stoppedWriteNote("the hand-on of this question", "made", {
        again: "Retry",
        method: "PUT",
        stopped: new SendStoppedError(),
      })
    );
  } finally {
    realClock();
    view.unmount();
    handAskTo.mockRestore();
    listUsers.mockRestore();
    getAsk.mockRestore();
  }
});

/** A fetch that failed, which the write is tried again after: it goes late, for the reader to
 *  stop. */
const dropped = (path: string) => new NetworkError(path, new TypeError("Failed to fetch"));

/** An open question on its card over the held clock, its answer write `answerAsk`: the reader
 *  picks Ship and presses Answer. */
async function answerOpenQuestion(answerAsk: () => Promise<Ask>) {
  const open = answeredAsk({ answer: null, state: "open" });
  const rendered = renderCard(
    <AskCard answerAsk={answerAsk} ask={open} initialThread={thread(open, [])} />
  );
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const card = within(rendered.view.container);
  await user.click(await card.findByRole("radio", { name: "Ship" }));
  await user.click(card.getByRole("button", { name: "Answer" }));
  return { ...rendered, card, open };
}

// An answer the reader stopped may have been saved: the card says so, and that its Retry asks
// again under the answer's key (`retryLast`), rather than a failure it cannot know of.
test("an answer the reader stopped says Retry asks again under its key, not that it failed", async () => {
  const { card, queryClient, view } = await answerOpenQuestion(() =>
    Promise.reject(dropped("/api/v1/asks/ask-1/answer"))
  );
  try {
    await stopTheLateWrite(queryClient);
    // The alert's paragraph, without its Retry button's name.
    expect(card.getByRole("alert").querySelector("p")?.textContent).toBe(
      stoppedWriteNote("your answer", "saved", {
        again: "Retry",
        method: "POST",
        stopped: new SendStoppedError(),
      })
    );
  } finally {
    realClock();
    view.unmount();
  }
});

// Once the question has closed the card shows its record and no form, so no Retry either: a
// stopped answer says what the card shows instead of promising a Retry it does not offer.
test("an answer stopped on a question that closed promises no Retry the card does not show", async () => {
  const { card, open, queryClient, view } = await answerOpenQuestion(() =>
    Promise.reject(dropped("/api/v1/asks/ask-1/answer"))
  );
  try {
    await stopTheLateWrite(queryClient);
    act(() => {
      queryClient.setQueryData<AskRead>(
        ["ask-thread", open.id],
        thread(answeredAsk({ answer: firstAnswer, state: "answered" }))
      );
    });
    await waitFor(() =>
      expect(card.queryByRole("button", { name: "Answer" })).not.toBeInTheDocument()
    );
    expect(card.getByRole("alert").textContent).toBe(
      stoppedWriteNote("your answer", "saved", {
        anew: "The question has closed, and the record above shows how.",
      })
    );
  } finally {
    realClock();
    view.unmount();
  }
});

// A question back the reader stopped may have been sent: the card says so, and that its Retry
// asks again under its key, rather than a failure it cannot know of.
test("a clarification the reader stopped says Retry asks again under its key, not that it failed", async () => {
  const createComment = spyOn(api, "createComment").mockRejectedValue(
    dropped("/api/v1/issues/CORE-1/comments")
  );
  const open = answeredAsk({ answer: null, state: "open" });
  const { queryClient, view } = renderCard(<AskCard ask={open} initialThread={thread(open, [])} />);
  const card = within(view.container);
  try {
    jest.useFakeTimers();
    const user = userOnFakeClock();
    const field = card.getByLabelText("Your answer");
    await user.click(field);
    await user.paste("Ship what, exactly?");
    await user.click(card.getByRole("button", { name: "Ask back" }));
    await stopTheLateWrite(queryClient);
    expect(card.getByRole("alert").querySelector("p")?.textContent).toBe(
      stoppedWriteNote("your clarification", "sent", {
        again: "Retry",
        method: "POST",
        stopped: new SendStoppedError(),
      })
    );
  } finally {
    realClock();
    view.unmount();
    createComment.mockRestore();
  }
});

// A reply the reader stopped on an answered question may have been posted: the composer says so,
// and that its Retry asks again under its key, rather than a failure it cannot know of.
test("a reply the reader stopped says Retry asks again under its key", async () => {
  const createComment = spyOn(api, "createComment").mockRejectedValue(
    dropped("/api/v1/issues/CORE-1/comments")
  );
  const answered = answeredAsk();
  const { queryClient, view } = renderCard(
    <AskCard ask={answered} initialThread={thread(answered)} />
  );
  const card = within(view.container);
  try {
    jest.useFakeTimers();
    const user = userOnFakeClock();
    await user.click(card.getByRole("textbox", { name: "Reply" }));
    await user.paste("Shipping it on Friday.");
    await user.click(card.getByRole("button", { name: "Reply" }));
    await stopTheLateWrite(queryClient);
    expect(card.getByRole("alert").querySelector("p")?.textContent).toBe(
      stoppedWriteNote("your reply", "posted", {
        again: "Retry",
        method: "POST",
        stopped: new SendStoppedError(),
      })
    );
  } finally {
    realClock();
    view.unmount();
    createComment.mockRestore();
  }
});
