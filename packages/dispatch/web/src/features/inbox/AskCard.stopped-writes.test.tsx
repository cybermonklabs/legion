import { beforeAll, expect, jest, mock, spyOn, test } from "bun:test";
// biome-ignore lint/style/noRestrictedImports: the pastes below carry a file, which user-event has no form for
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { realClock, tick } from "../../__tests__/fake-clock";
import { openList } from "../../__tests__/list-picks";
import { stopTheLateWrite, untilAttempted } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { api, NetworkError, type WriteBounds } from "../../api/client";
import type { AnswerAskInput, Ask, AskRead, Comment } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { AskCard } from "./AskCard";
import { answerDraft, setAnswerDraft } from "./answer-drafts";
import {
  answeredAsk,
  firstAnswer,
  installAskCardApiMocks,
  renderCard,
  thread,
} from "./ask-card-fixture";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

// A write the reader stopped on the card: a hand-on, an answer, a question back and a reply may
// each have been made, so the card says what its Retry does rather than that the write failed;
// and what that Retry is of once a host draws the same card for another ask.

installAskCardApiMocks();
// The card draws a question's text and its options through the Markdown engine, a module loaded
// on first use (`markdown-engine.ts`). Most rows here hold the clock before they find an option,
// and a find on a held clock spends its wait in moments of that clock, which a load on the real
// one does not keep pace with. So the engine is loaded before the rows begin.
beforeAll(warmMarkdownRenderer);

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
    // The people are read only once the reader reaches for the control (`AskHandOn`).
    const list = await openList(userEvent.setup(), "Hand this question to");
    await waitFor(() => expect(within(list).getByRole("option", { name: "bob" })).toBeTruthy());
    jest.useFakeTimers();
    await userOnFakeClock().click(within(list).getByRole("option", { name: "bob" }));
    await stopTheLateWrite(queryClient);

    // The alert's paragraph, without its Retry button's name.
    expect(card.getByRole("alert").querySelector("p")?.textContent).toBe(
      stoppedWriteNote("the hand-on of this question", "made", {
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
async function answerOpenQuestion(
  answerAsk: (id: string, input: AnswerAskInput, bounds: WriteBounds) => Promise<Ask>
) {
  const open = answeredAsk({ answer: null, state: "open" });
  const rendered = renderCard(
    <AskCard answerAsk={answerAsk} ask={open} initialThread={thread(open, [])} />
  );
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const card = within(rendered.view.container);
  // The who-am-I read settles here, in a wait that moves the held clock inside `act`. The radio
  // below is found at once, and the wait for it ends before the read's answer lands, which would
  // then update the card outside `act`.
  await waitFor(() =>
    expect(rendered.queryClient.getQueryState(["whoami"])?.status).toBe("success")
  );
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

/** The question a host draws the same card for next (`ThreadBody` draws one card for one ask
 *  after another): another open ask, on another issue. */
function nextQuestion(overrides: Partial<Ask> = {}): Ask {
  return answeredAsk({
    answer: null,
    id: "ask-2",
    issue_key: "CORE-2",
    question: "Which window?",
    state: "open",
    ...overrides,
  });
}

/** Presses the Retry of the card's one refusal, on the clock the case holds. */
async function retryTheRefusal() {
  await userOnFakeClock().click(
    within(screen.getByRole("alert")).getByRole("button", { name: "Retry" })
  );
  await tick(1_000);
}

/** An attempt Dispatch answers with `answer` once the clock the case holds moves on, so what the
 *  landing does to the card runs inside that move (`tick`), not inside the click that made the
 *  attempt, which user-event lets run outside `act`. */
function answersOnTheClock<T>(answer: T): () => Promise<T> {
  return () => {
    const { promise, resolve } = Promise.withResolvers<T>();
    setTimeout(() => resolve(answer), 100);
    return promise;
  };
}

// A hand-on's refusal stays on the card as the card moves to the next ask, and its Retry is still
// that hand-on: it hands the first ask to the person picked for it, not the ask the card shows.
test("a stopped hand-on's Retry after the card moves to another ask hands on the ask it was pressed on", async () => {
  const open = answeredAsk({ answer: null, state: "open" });
  const next = nextQuestion();
  const handAskTo = spyOn(api, "handAskTo").mockRejectedValue(
    dropped("/api/v1/asks/ask-1/asked-of")
  );
  const listUsers = spyOn(api, "listUsers").mockResolvedValue([
    { login: "alice" },
    { login: "bob" },
  ]);
  // The hand-on reads the ask's thread again once it settles, stopped or not.
  const getAsk = spyOn(api, "getAsk").mockResolvedValue(thread(open, []));
  const { queryClient, rerender, view } = renderCard(
    <AskCard ask={open} initialThread={thread(open, [])} />
  );
  try {
    // The people are read only once the reader reaches for the control (`AskHandOn`).
    const list = await openList(userEvent.setup(), "Hand this question to");
    await waitFor(() => expect(within(list).getByRole("option", { name: "bob" })).toBeTruthy());
    jest.useFakeTimers();
    await userOnFakeClock().click(within(list).getByRole("option", { name: "bob" }));
    await stopTheLateWrite(queryClient);
    const stopped = handAskTo.mock.calls.length;

    rerender(<AskCard ask={next} initialThread={thread(next, [])} />);
    handAskTo.mockResolvedValue({ ...open, asked_of: "bob" });
    await retryTheRefusal();

    expect(handAskTo).toHaveBeenCalledTimes(stopped + 1);
    const again = handAskTo.mock.calls.at(-1);
    expect(again?.[0]).toBe("ask-1");
    expect(again?.[1]).toBe("bob");
  } finally {
    realClock();
    view.unmount();
    handAskTo.mockRestore();
    listUsers.mockRestore();
    getAsk.mockRestore();
  }
});

// So does an answer: its Retry sends the stopped answer to the first ask under its key. Sent to
// the ask the card shows, it would answer that ask with the first one's answer, or be refused
// `422 KEY_REUSED` where the first had reached Dispatch, since the key covers the path.
test("a stopped answer's Retry after the card moves to another ask answers the ask it was pressed on", async () => {
  const answerAsk = mock((_id: string, _input: AnswerAskInput, _bounds: WriteBounds) =>
    Promise.reject<Ask>(dropped("/api/v1/asks/ask-1/answer"))
  );
  const { open, queryClient, rerender, view } = await answerOpenQuestion(answerAsk);
  try {
    await stopTheLateWrite(queryClient);
    const stoppedKey = answerAsk.mock.calls[0]?.[2].key;
    const stopped = answerAsk.mock.calls.length;

    const next = nextQuestion();
    rerender(<AskCard answerAsk={answerAsk} ask={next} initialThread={thread(next, [])} />);
    answerAsk.mockImplementation(
      answersOnTheClock({ ...open, answer: firstAnswer, state: "answered" })
    );
    await retryTheRefusal();

    expect(answerAsk).toHaveBeenCalledTimes(stopped + 1);
    const again = answerAsk.mock.calls.at(-1);
    expect(again?.[0]).toBe("ask-1");
    expect(again?.[1]).toEqual({ expected_edited_at: null, selected: ["Ship"] });
    expect(again?.[2].key).toBe(stoppedKey);
  } finally {
    realClock();
    view.unmount();
  }
});

// What an answer's landing does to the card is its own ask's alone. A card the host has drawn
// for the next ask by then keeps that ask's form and the draft the reader wrote for it, and the
// host is told the first ask was answered. Set on the card it moved to, the landing would show
// the first ask's answer over the next question and take away the box its draft is in.
test("an answer that lands after the card moves to another ask leaves that ask's form and draft alone", async () => {
  const open = answeredAsk({ answer: null, state: "open" });
  const next = nextQuestion();
  const lands = answersOnTheClock<Ask>({ ...open, answer: firstAnswer, state: "answered" });
  const answerAsk = mock((_id: string, _input: AnswerAskInput, _bounds: WriteBounds) => lands());
  const onAnswered = mock((_id: string) => {});
  const { queryClient, rerender, view } = renderCard(
    <AskCard
      answerAsk={answerAsk}
      ask={open}
      initialThread={thread(open, [])}
      onAnswered={onAnswered}
    />
  );
  try {
    jest.useFakeTimers();
    const user = userOnFakeClock();
    // The who-am-I read settles here, as in `answerOpenQuestion`.
    await waitFor(() => expect(queryClient.getQueryState(["whoami"])?.status).toBe("success"));
    await user.click(await screen.findByRole("radio", { name: "Ship" }));
    await user.click(screen.getByRole("button", { name: "Answer" }));

    setAnswerDraft(next.id, "Hold until Friday");
    rerender(
      <AskCard
        answerAsk={answerAsk}
        ask={next}
        initialThread={thread(next, [])}
        onAnswered={onAnswered}
      />
    );
    await tick(1_000);

    expect(answerAsk).toHaveBeenCalledTimes(1);
    expect(answerAsk.mock.calls[0]?.[0]).toBe("ask-1");
    expect(onAnswered.mock.calls).toEqual([["ask-1"]]);
    expect(screen.queryAllByText(open.question).length).toBe(0);
    expect(screen.getByText(next.question)).toBeInTheDocument();
    expect(screen.getByLabelText<HTMLTextAreaElement>("Your answer").value).toBe(
      "Hold until Friday"
    );
    expect(answerDraft(next.id)).toBe("Hold until Friday");
  } finally {
    realClock();
    view.unmount();
  }
});

// A question back's Retry posts the stopped text on the first ask's issue, under its key, and its
// landing drops that text from the first ask's draft, where it was written.
test("a stopped question back's Retry after the card moves to another ask posts on the ask it was pressed on", async () => {
  const createComment = spyOn(api, "createComment").mockRejectedValue(
    dropped("/api/v1/issues/CORE-1/comments")
  );
  const open = answeredAsk({ answer: null, state: "open" });
  const next = nextQuestion();
  const { queryClient, rerender, view } = renderCard(
    <AskCard ask={open} initialThread={thread(open, [])} />
  );
  try {
    jest.useFakeTimers();
    const user = userOnFakeClock();
    await user.click(screen.getByLabelText("Your answer"));
    await user.paste("Ship what, exactly?");
    await user.click(screen.getByRole("button", { name: "Ask back" }));
    await stopTheLateWrite(queryClient);
    const stoppedKey = createComment.mock.calls[0]?.[2].key;
    const stopped = createComment.mock.calls.length;

    rerender(<AskCard ask={next} initialThread={thread(next, [])} />);
    createComment.mockImplementation(answersOnTheClock({} as Comment));
    await retryTheRefusal();

    expect(createComment).toHaveBeenCalledTimes(stopped + 1);
    const again = createComment.mock.calls.at(-1);
    expect(again?.[0]).toBe("CORE-1");
    expect(again?.[1]).toEqual({ ask_id: "ask-1", body: "Ship what, exactly?" });
    expect(again?.[2].key).toBe(stoppedKey);
    expect(answerDraft("ask-1")).toBe("");
  } finally {
    realClock();
    view.unmount();
    createComment.mockRestore();
  }
});

/** A picture pasted into a draft, and what Dispatch answers its upload with. */
const shot = new File(["bytes"], "shot.png", { type: "image/png" });
const SHOT_STORED = {
  artifact: { kind: "image", name: "shot.png", slug: "shot-png" } as never,
  version: { number: 1 } as never,
  previous_version: null,
};
const SHOT_TEXT = "![shot.png](dispatch://CORE-1/artifact/shot-png@v1)";

// An upload's text joins the draft the file was pasted into: made again by Retry once the card
// shows another ask, it goes into the first ask's answer, not into the answer on screen.
test("a stopped upload's Retry after the card moves to another ask adds the file to the answer it was pasted into", async () => {
  const uploadArtifact = spyOn(api, "uploadArtifact").mockRejectedValue(
    dropped("/api/v1/issues/CORE-1/artifacts")
  );
  const open = answeredAsk({ answer: null, state: "open" });
  const next = nextQuestion();
  const { queryClient, rerender, view } = renderCard(
    <AskCard ask={open} initialThread={thread(open, [])} />
  );
  try {
    jest.useFakeTimers();
    // user-event's paste carries text, never the file this field uploads.
    fireEvent.paste(screen.getByLabelText("Your answer"), { clipboardData: { files: [shot] } });
    await tick(1_000);
    await untilAttempted(uploadArtifact, 1);
    await stopTheLateWrite(queryClient);
    const stoppedKey = uploadArtifact.mock.calls[0]?.[2].key;
    const stopped = uploadArtifact.mock.calls.length;

    rerender(<AskCard ask={next} initialThread={thread(next, [])} />);
    uploadArtifact.mockResolvedValue(SHOT_STORED);
    await retryTheRefusal();
    await untilAttempted(uploadArtifact, stopped + 1);
    await tick(1_000);

    expect(uploadArtifact).toHaveBeenCalledTimes(stopped + 1);
    const again = uploadArtifact.mock.calls.at(-1);
    expect(again?.[0]).toEqual({ issue: "CORE-1" });
    expect(again?.[2].key).toBe(stoppedKey);
    expect(answerDraft("ask-1")).toBe(SHOT_TEXT);
    expect(screen.getByLabelText("Your answer")).toHaveValue("");
  } finally {
    realClock();
    view.unmount();
    uploadArtifact.mockRestore();
  }
});

// The same under a reply to an answered ask: the file's text joins the first ask's reply draft,
// which its composer shows once the card is drawn for that ask again.
test("a stopped upload's Retry after the card moves to another ask adds the file to the reply it was pasted into", async () => {
  const uploadArtifact = spyOn(api, "uploadArtifact").mockRejectedValue(
    dropped("/api/v1/issues/CORE-1/artifacts")
  );
  const answered = answeredAsk();
  const next = nextQuestion({ answer: firstAnswer, state: "answered" });
  const { queryClient, rerender, view } = renderCard(
    <AskCard ask={answered} initialThread={thread(answered)} />
  );
  try {
    jest.useFakeTimers();
    // user-event's paste carries text, never the file this field uploads.
    fireEvent.paste(screen.getByRole("textbox", { name: "Reply" }), {
      clipboardData: { files: [shot] },
    });
    await tick(1_000);
    await untilAttempted(uploadArtifact, 1);
    await stopTheLateWrite(queryClient);
    const stoppedKey = uploadArtifact.mock.calls[0]?.[2].key;
    const stopped = uploadArtifact.mock.calls.length;

    rerender(<AskCard ask={next} initialThread={thread(next)} />);
    uploadArtifact.mockResolvedValue(SHOT_STORED);
    await retryTheRefusal();
    await untilAttempted(uploadArtifact, stopped + 1);
    await tick(1_000);

    expect(uploadArtifact).toHaveBeenCalledTimes(stopped + 1);
    const again = uploadArtifact.mock.calls.at(-1);
    expect(again?.[0]).toEqual({ issue: "CORE-1" });
    expect(again?.[2].key).toBe(stoppedKey);
    expect(screen.getByRole("textbox", { name: "Reply" })).toHaveValue("");
    rerender(<AskCard ask={answered} initialThread={thread(answered)} />);
    expect(screen.getByRole("textbox", { name: "Reply" })).toHaveValue(SHOT_TEXT);
  } finally {
    realClock();
    view.unmount();
    uploadArtifact.mockRestore();
  }
});
