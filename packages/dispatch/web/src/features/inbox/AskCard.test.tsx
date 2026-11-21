import { expect, jest, spyOn, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, render, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { dwellOn, installIntersectionObserver } from "../../__tests__/intersection-observer";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { highlightedRow, listOf, textBoxes } from "../../__tests__/list-picks";
import { expectSameElements } from "../../__tests__/same-elements";
import { answerThreadReads } from "../../__tests__/send-answers";
import { ApiError, api } from "../../api/client";
import { askThreadQuery, myRepliesOwnerQuery, usersQuery } from "../../api/queries";
import { DispatchQueryClient } from "../../api/query-client";
import type {
  AnswerAskInput,
  Ask,
  AskAnswer,
  AskRead,
  Comment,
  CreateCommentInput,
} from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { userPreferenceStorageKey } from "../shell/userPreference";
import { replyRow, seedUnread } from "../threads/replies-fixture";
import { AskCard } from "./AskCard";
import { answerDraft } from "./answer-drafts";
import {
  answeredAsk,
  firstAnswer,
  installAskCardApiMocks,
  renderCard,
  thread,
} from "./ask-card-fixture";
import { ThreadBody } from "./ThreadBody";

installAskCardApiMocks();

test("the answerer can change an answer and sees both answers immediately", async () => {
  const user = userEvent.setup();
  const before = answeredAsk();
  const secondAnswer: AskAnswer = {
    at: "2026-10-08T02:00:00Z",
    selected: ["Hold"],
    text: null,
    user: "alice",
  };
  const after = answeredAsk({ answer: secondAnswer });
  const sent: AnswerAskInput[] = [];
  let threadReads = 0;
  const refreshed = Promise.withResolvers<AskRead>();
  const getAskThread = async () => {
    threadReads += 1;
    return refreshed.promise;
  };
  const { view } = renderCard(
    <AskCard
      answerAsk={async (_id, input) => {
        sent.push(input);
        return after;
      }}
      ask={before}
      getAskThread={getAskThread}
      initialThread={thread(before)}
    />
  );

  try {
    await user.click(await view.findByRole("button", { name: "Change answer" }));
    const ship = (await view.findByRole("radio", { name: "Ship" })) as HTMLInputElement;
    expect(ship.checked).toBe(true);
    expect((view.getByLabelText("Your answer") as HTMLTextAreaElement).value).toBe("First note");
    await user.click(view.getByRole("radio", { name: "Hold" }));
    await user.clear(view.getByLabelText("Your answer"));
    await user.click(view.getByRole("button", { name: "Save answer" }));

    await waitFor(() =>
      expect(sent).toEqual([
        {
          expected_answer_at: firstAnswer.at,
          expected_edited_at: null,
          selected: ["Hold"],
        },
      ])
    );
    await waitFor(() => expect(view.getByText("Show 1 earlier answer")).toBeTruthy());
    expect(threadReads).toBe(1);
    await user.click(view.getByText("Show 1 earlier answer"));
    expect(view.getByText("First note")).toBeTruthy();
    expect(
      within(view.getByTestId("ask-ask-1")).getAllByRole("listitem")[1]?.dataset.selected
    ).toBe("true");
    expect(threadReads).toBe(1);
    refreshed.resolve(thread(after, [firstAnswer, secondAnswer]));
    await waitFor(() => expect(view.getByText("Show 1 earlier answer")).toBeTruthy());
  } finally {
    view.unmount();
  }
});

// A read of the thread asked before a changed answer landed - an event's refresh of the ask, say
// - answers the thread as it stood, with the first answer alone. Landing after the change, it
// would take the earlier answer away from under the new one, until the change's own refresh came
// back. On Dispatch's client that refresh waits for the running read rather than cancelling it,
// so the row reads on that client. The change cancels that read and asks the thread again
// (`replaceQueryData`), and its own refresh waits for that read, so two reads asked after the
// change answer the thread as it stands now.
test("a read of the thread asked before a changed answer landed does not put back the answers it replaced", async () => {
  const user = userEvent.setup();
  const before = answeredAsk();
  const secondAnswer: AskAnswer = {
    at: "2026-10-08T02:00:00Z",
    selected: ["Hold"],
    text: null,
    user: "alice",
  };
  const after = answeredAsk({ answer: secondAnswer });
  const reads: ReturnType<typeof Promise.withResolvers<AskRead>>[] = [];
  const getAskThread = () => {
    const read = Promise.withResolvers<AskRead>();
    reads.push(read);
    return read.promise;
  };
  const { queryClient, view } = renderCard(
    <AskCard
      answerAsk={async () => after}
      ask={before}
      getAskThread={getAskThread}
      initialThread={thread(before)}
    />,
    new DispatchQueryClient({
      defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
    })
  );
  const history = () =>
    within(view.getByTestId("ask-ask-1")).queryByText(/earlier answer/)?.textContent;
  try {
    const change = await view.findByRole("button", { name: "Change answer" });
    let asked = Promise.resolve();
    await act(async () => {
      asked = queryClient.refetchQueries({ queryKey: ["ask-thread", before.id] });
    });
    expect(reads).toHaveLength(1);

    await user.click(change);
    await user.click(await view.findByRole("radio", { name: "Hold" }));
    await user.clear(view.getByLabelText("Your answer"));
    await user.click(view.getByRole("button", { name: "Save answer" }));
    await waitFor(() => expect(history()).toBe("Show 1 earlier answer"));

    // The read answers as Dispatch held the thread when it was asked: the first answer alone.
    await act(async () => {
      reads[0]?.resolve(thread(before));
      await asked;
    });
    expect(history()).toBe("Show 1 earlier answer");

    // The change's read again, then its own refresh, which waits for that read: both asked after
    // the change, so both answer the thread as it stands now.
    const now = thread(after, [firstAnswer, secondAnswer]);
    await waitFor(() => expect(reads).toHaveLength(2));
    await act(async () => {
      reads[1]?.resolve(now);
    });
    await waitFor(() => expect(reads).toHaveLength(3));
    await act(async () => {
      reads[2]?.resolve(now);
    });
    await waitFor(() => expect(queryClient.isFetching()).toBe(0));
    expect(reads).toHaveLength(3);
    expect(history()).toBe("Show 1 earlier answer");
  } finally {
    view.unmount();
  }
});

/** Reads an ask's thread with no seed of its own, as the reading column's thread does
 *  (`ThreadPane`). */
function UnseededThreadRead({ id }: { id: string }): ReactNode {
  useQuery(askThreadQuery(id));
  return null;
}

// A read of the thread asked before the Inbox read that seeds a card - the reading column's, say,
// which reads the thread with no seed - answers the thread as it stood then, with the ask still
// open. TanStack puts the card's seed into that read's query, which holds no data yet, and leaves
// the read running, so its answer would replace the newer seed with the older ask. The card keeps
// its seed until a read asked after the seed answers.
test("a read of the thread asked before the card's seed does not put back the ask it replaced", async () => {
  const open = answeredAsk({ answer: null, state: "open" });
  const secondAnswer: AskAnswer = {
    at: "2026-10-08T02:00:00Z",
    selected: ["Hold"],
    text: null,
    user: "alice",
  };
  const thirdAnswer: AskAnswer = {
    at: "2026-10-08T03:00:00Z",
    selected: ["Ship"],
    text: "Third note",
    user: "alice",
  };
  const answered = answeredAsk({ answer: secondAnswer });
  const current = answeredAsk({ answer: thirdAnswer });
  const reads: ReturnType<typeof Promise.withResolvers<AskRead>>[] = [];
  const getAsk = spyOn(api, "getAsk").mockImplementation(() => {
    const read = Promise.withResolvers<AskRead>();
    reads.push(read);
    return read.promise;
  });
  const queryClient = new DispatchQueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const page = (card?: ReactNode) => (
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <UnseededThreadRead id={open.id} />
        {card}
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(page());
  const history = () =>
    within(view.getByTestId("ask-ask-1")).queryByText(/earlier answer/)?.textContent;
  try {
    await waitFor(() => expect(reads).toHaveLength(1));
    const olderRead = queryClient
      .getQueryCache()
      .find({ exact: true, queryKey: ["ask-thread", open.id] })?.promise;

    // The card mounts from an Inbox read that answered after that read was asked: the ask is
    // answered there, and its answer changed once.
    view.rerender(
      page(<AskCard ask={answered} initialThread={thread(answered, [firstAnswer, secondAnswer])} />)
    );
    await waitFor(() => expect(history()).toBe("Show 1 earlier answer"));

    // The older read answers the ask as it stood when it was asked.
    await act(async () => {
      reads[0]?.resolve(thread(open, []));
      await olderRead?.catch(() => undefined);
    });
    expect(history()).toBe("Show 1 earlier answer");

    // A read asked after the seed answers the thread as it stands now.
    await waitFor(() => expect(reads).toHaveLength(2));
    await act(async () => {
      reads[1]?.resolve(thread(current, [firstAnswer, secondAnswer, thirdAnswer]));
    });
    await waitFor(() => expect(history()).toBe("Show 2 earlier answers"));
  } finally {
    view.unmount();
    getAsk.mockRestore();
  }
});

test("Change answer is offered only to the person who gave a non-approval answer", async () => {
  const bob = renderCard(
    <AskCard
      ask={answeredAsk({ answer: { ...firstAnswer, user: "bob" } })}
      initialThread={thread(answeredAsk({ answer: { ...firstAnswer, user: "bob" } }))}
    />
  );
  try {
    await waitFor(() => expect(bob.view.getByText(/Answered by/)).toBeTruthy());
    expect(bob.view.queryByRole("button", { name: "Change answer" })).not.toBeInTheDocument();
  } finally {
    bob.view.unmount();
  }

  const approval = answeredAsk({
    kind: "approval",
    options: [{ label: "Approve" }, { label: "Request changes" }],
  });
  const review = renderCard(<AskCard ask={approval} initialThread={thread(approval)} />);
  try {
    await waitFor(() => expect(review.view.getByText(/Answered by/)).toBeTruthy());
    expect(review.view.queryByRole("button", { name: "Change answer" })).not.toBeInTheDocument();
  } finally {
    review.view.unmount();
  }
});

test("a stale change says the answer changed", async () => {
  const user = userEvent.setup();
  const before = answeredAsk();
  // The refusal sends the card back to the server for the answer of record, which is the one it
  // was already showing.
  const getAsk = spyOn(api, "getAsk").mockResolvedValue(thread(before));
  const { view } = renderCard(
    <AskCard
      answerAsk={async () => {
        throw new ApiError(409, { code: "ASK_ANSWER_CHANGED", error: "answer changed" });
      }}
      ask={before}
      initialThread={thread(before)}
    />
  );

  try {
    await user.click(await view.findByRole("button", { name: "Change answer" }));
    // "Hold" is a Markdown label (`MarkdownBody`), unnamed until the lazy Markdown engine loads.
    await user.click(await view.findByRole("radio", { name: "Hold" }));
    await user.click(view.getByRole("button", { name: "Save answer" }));
    expect(
      await view.findByText("The answer changed since you opened it; here is the current one.")
    ).toBeTruthy();
    expect(view.queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
  } finally {
    view.unmount();
    getAsk.mockRestore();
  }
});

for (const [code, message] of [
  ["NOT_ANSWERER", "Only the person who answered can change this answer."],
  ["ASK_APPROVAL_REVIEW", "An approval is a review; record a new review on the document."],
] as const) {
  test(`a change refused ${code} closes the form and shows the recorded answer`, async () => {
    const user = userEvent.setup();
    const before = answeredAsk();
    // The refusal sends the card back to the server for the answer of record.
    const getAsk = spyOn(api, "getAsk").mockResolvedValue(thread(before));
    const { view } = renderCard(
      <AskCard
        answerAsk={async () => {
          throw new ApiError(409, { code, error: "refused" });
        }}
        ask={before}
        initialThread={thread(before)}
      />
    );
    try {
      await user.click(await view.findByRole("button", { name: "Change answer" }));
      // "Hold" is a Markdown label (`MarkdownBody`), unnamed until the lazy Markdown engine loads.
      await user.click(await view.findByRole("radio", { name: "Hold" }));
      await user.click(view.getByRole("button", { name: "Save answer" }));
      expect(await view.findByText(message)).toBeTruthy();
      // The refusal meets the same send again, so the form it was sent from closes.
      expect(view.queryByRole("button", { name: "Save answer" })).not.toBeInTheDocument();
      expect(view.getByText(/Answered by/)).toBeTruthy();
      expect(view.queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
    } finally {
      view.unmount();
      getAsk.mockRestore();
    }
  });
}

test("mode change opens the seeded form only when the viewer can change the answer", async () => {
  const mine = answeredAsk();
  const editable = renderCard(<AskCard ask={mine} initialThread={thread(mine)} mode="change" />);
  try {
    expect(await editable.view.findByRole("button", { name: "Save answer" })).toBeTruthy();
    expect(editable.view.queryByText(/Answered by/)).not.toBeInTheDocument();
    // "Ship" is a Markdown label (`MarkdownBody`), unnamed until the lazy Markdown engine loads.
    const ship = (await editable.view.findByRole("radio", { name: "Ship" })) as HTMLInputElement;
    expect(ship.checked).toBe(true);
  } finally {
    editable.view.unmount();
  }

  const theirs = answeredAsk({ answer: { ...firstAnswer, user: "bob" } });
  const readOnly = renderCard(
    <AskCard ask={theirs} initialThread={thread(theirs)} mode="change" />
  );
  try {
    await waitFor(() => expect(readOnly.view.getByText(/Answered by/)).toBeTruthy());
    expect(readOnly.view.queryByRole("button", { name: "Save answer" })).not.toBeInTheDocument();
  } finally {
    readOnly.view.unmount();
  }
});

test("a compact card's change shows the seeded note, so nothing is sent unseen", async () => {
  const user = userEvent.setup();
  const mine = answeredAsk();
  const { view } = renderCard(
    <AskCard
      ask={mine}
      frame="block"
      initialThread={thread(mine)}
      thread="collapsed"
      variant="compact"
    />
  );
  try {
    await user.click(await view.findByRole("button", { name: "Change answer" }));
    const note = (await view.findByLabelText("Your answer")) as HTMLTextAreaElement;
    expect(note.value).toBe("First note");
    expect(view.getByRole("button", { name: "Save answer" })).toBeTruthy();
    expect(view.getByRole("button", { name: "Cancel" })).toBeTruthy();
  } finally {
    view.unmount();
  }
});

test("Change answer then Cancel leaves the recorded answer alone and no draft behind", async () => {
  const user = userEvent.setup();
  const mine = answeredAsk();
  const { view } = renderCard(<AskCard ask={mine} initialThread={thread(mine)} />);
  const card = within(view.container);
  try {
    await user.click(await view.findByRole("button", { name: "Change answer" }));
    expect((card.getByLabelText("Your answer") as HTMLTextAreaElement).value).toBe("First note");
    await user.click(card.getByRole("button", { name: "Cancel" }));

    // The card is back to the recorded answer, and the reader's own answer is not handed back
    // to them as text no form will send.
    expect(await card.findByText(/Answered by/)).toBeTruthy();
    expect(card.queryByTestId("ask-unsent-answer")).not.toBeInTheDocument();
    // Nor is any of it left in the draft every card for this ask reads, and the Inbox's reading
    // column holds the question in the column by (`pane-selection.ts`, `paneAsk`).
    expect(answerDraft(mine.id)).toBe("");
  } finally {
    view.unmount();
  }
});

function askReply(id: string, createdAt: string): Comment {
  return {
    anchor: null,
    ask_id: "ask-1",
    author: { id: "session-1", kind: "session" },
    body: `Reply ${id}`,
    created_at: createdAt,
    edited_at: null,
    id,
    issue_key: "CORE-1",
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
    ...commentDeliveryFields(),
  };
}

/** An answered ask with two replies, unread for alice on CORE-1, rendered with time and the
 *  screen in the test's hands: the replies follow the first answer, or precede `answer` when one
 *  is given. */
async function renderUnreadAsk(props: {
  markRead: boolean;
  thread: "inline" | "collapsed";
  answer?: AskAnswer;
}) {
  // A reply is Markdown (`MarkdownBody`), empty until the lazy Markdown engine loads in real time.
  await warmMarkdownRenderer();
  jest.useFakeTimers();
  const screen = installIntersectionObserver();
  const mark = answerThreadReads().mockResolvedValue(undefined);
  const ask = answeredAsk(props.answer === undefined ? {} : { answer: props.answer });
  const replies = [
    askReply("reply-1", "2026-10-08T03:00:00Z"),
    askReply("reply-2", "2026-10-08T04:00:00.5Z"),
  ];
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  seedUnread(queryClient, { issue: "CORE-1" }, [ask.id]);
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <AskCard
          ask={ask}
          initialThread={{ ...thread(ask), replies }}
          markRead={props.markRead}
          thread={props.thread}
          variant="compact"
        />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    cleanup: () => {
      view.unmount();
      mark.mockRestore();
      screen.restore();
    },
    mark,
    screen,
    view,
  };
}

for (const shape of ["inline", "collapsed"] as const) {
  test(`with markRead, the ${shape} thread is marked read once its newest reply is on screen`, async () => {
    const card = await renderUnreadAsk({ markRead: true, thread: shape });
    try {
      const newest = (await card.view.findByText("Reply reply-2")).closest("li");
      if (newest === null) throw new Error("expected the newest reply's item");
      expectSameElements(card.screen.observed(), [newest]);
      await dwellOn(card.screen, newest);
      expect(card.mark.mock.calls).toEqual([
        ["ask-1", { read_through: "2026-10-08T04:00:00.5Z" }, keyedAttempt],
      ]);
    } finally {
      card.cleanup();
    }
  });

  test(`without markRead, the ${shape} thread watches nothing`, async () => {
    const card = await renderUnreadAsk({ markRead: false, thread: shape });
    try {
      await card.view.findByText("Reply reply-2");
      expect(card.screen.observed().length).toBe(0);
      expect(card.mark).not.toHaveBeenCalled();
    } finally {
      card.cleanup();
    }
  });
}

// An answer is a write like a reply: when someone answers after every reply, the answered record is
// the newest write the card shows, so the mark watches the record and reads through the answer.
test("an answer given after every reply is what the mark watches and reads through", async () => {
  const answer: AskAnswer = {
    at: "2026-10-08T05:00:00.123456789Z",
    selected: ["Hold"],
    text: "Wait for the fix.",
    user: "bob@example.com",
  };
  const card = await renderUnreadAsk({ answer, markRead: true, thread: "collapsed" });
  try {
    const record = await card.view.findByTestId("ask-ask-1");
    await card.view.findByText("Reply reply-2");
    expectSameElements(card.screen.observed(), [record]);
    await dwellOn(card.screen, record);
    expect(card.mark.mock.calls).toEqual([["ask-1", { read_through: answer.at }, keyedAttempt]]);
  } finally {
    card.cleanup();
  }
});

/** One of the decision-block fixtures `cutDecisionBlocks` and the Go server's cut share. */
function decisionBlockFixture(name: string): string {
  return readFileSync(
    join(import.meta.dir, "../../../../../contracts/fixtures/decision-blocks", name),
    "utf8"
  );
}

const earlierApproval = {
  approved: { at: "2026-10-05T14:30:00Z", by: { id: "alice", kind: "user" }, version: 11 },
} satisfies Partial<NonNullable<Ask["approval"]>>;

/** An open approval request for version 12 of `spec.md`, carrying `approval` as given. */
function approvalAsk(approval: Partial<NonNullable<Ask["approval"]>> = {}): Ask {
  return answeredAsk({
    answer: null,
    approval: {
      artifact_id: "artifact-1",
      documents: null,
      name: "spec.md",
      requested_version: 12,
      version: 12,
      ...approval,
    },
    approval_artifact: { name: "spec.md", primary: true, project: "CORE", slug: "spec-md" },
    kind: "approval",
    options: [{ label: "Approve" }, { label: "Request changes" }],
    question: "Approve spec.md (version 12)?",
    state: "open",
  });
}

/** `getArtifactVersion` answering `versions[n]` as version n's markdown. */
function mockVersions(versions: Record<number, string>) {
  return spyOn(api, "getArtifactVersion").mockImplementation(async (_id, version) => ({
    authors: [],
    created_at: "2026-10-05T00:00:00Z",
    markdown: versions[version] ?? "",
    named: false,
    number: version,
    summary: null,
  }));
}

// The suite's preload registers no testing-library cleanup, so a render another file never
// unmounts stays in the shared document; these tests read their own card alone (`within`).
for (const variant of ["full", "compact"] as const) {
  test(`a ${variant} approval card names the earlier approval and diffs the two versions only when asked`, async () => {
    // The question is read in the commit that mounts the card, which only a loaded engine draws
    // (`MarkdownBody`): the first body of a file takes the asynchronous path.
    await warmMarkdownRenderer();
    const user = userEvent.setup();
    const ask = approvalAsk(earlierApproval);
    const getArtifactVersion = mockVersions({ 11: "Before\n", 12: "After\n" });
    const { view } = renderCard(
      variant === "full" ? (
        <AskCard ask={ask} initialThread={thread(ask)} />
      ) : (
        <AskCard ask={ask} documentLink={false} initialThread={thread(ask)} variant="compact" />
      )
    );
    const card = within(view.container);
    try {
      const line = card.getByText(/^Approved at version 11 on/);
      expect(line.textContent).toBe("Approved at version 11 on 2026-10-05 by alice.");
      const time = line.querySelector("time");
      expect(time?.getAttribute("datetime")).toBe("2026-10-05T14:30:00Z");
      expect(time?.getAttribute("title")).toBe("2026-10-05T14:30:00Z");
      expect(card.getByText("Approve spec.md (version 12)?")).toBeTruthy();
      expect(card.queryByTestId("change-view")).not.toBeInTheDocument();
      expect(getArtifactVersion).not.toHaveBeenCalled();

      const show = card.getByRole("button", { name: "Show diff" });
      expect(show).toHaveAttribute("aria-expanded", "false");
      await user.click(show);
      const diff = await card.findByTestId("change-view");
      expect(getArtifactVersion.mock.calls).toEqual([
        ["artifact-1", 11],
        ["artifact-1", 12],
      ]);
      expect(diff.querySelector("del")?.textContent).toBe("Before");
      expect(diff.querySelector("ins")?.textContent).toBe("After");
      expect(card.queryByRole("button", { name: "Show diff" })).not.toBeInTheDocument();

      const hide = card.getByRole("button", { name: "Hide diff" });
      expect(hide).toHaveAttribute("aria-expanded", "true");
      await user.click(hide);
      expect(card.queryByTestId("change-view")).not.toBeInTheDocument();
      expect(card.getByRole("button", { name: "Show diff" })).toBeTruthy();
    } finally {
      view.unmount();
      getArtifactVersion.mockRestore();
    }
  });
}

test("a first approval request shows no earlier approval and no diff control", async () => {
  const ask = approvalAsk();
  const { view } = renderCard(<AskCard ask={ask} initialThread={thread(ask)} />);
  const card = within(view.container);
  try {
    await card.findByRole("radio", { name: "Approve" });
    expect(card.getByText("Approval requested")).toBeTruthy();
    expect(card.queryByText(/Approved at version/)).not.toBeInTheDocument();
    expect(card.queryByRole("button", { name: "Show diff" })).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

// The compact card - the margin's, the review sheet's - marks its approval options for the keys
// as the full one does, and its Request changes opens the reason behind its disclosure. Escape
// there lets go of the pick and leaves the box open on what was typed, so a focus a host moves
// on the same key is not dropped by a box unmounting under it.
test("a compact approval card marks its options, and Escape in its reason drops the Request changes pick", async () => {
  const user = userEvent.setup();
  const ask = approvalAsk();
  const { view } = renderCard(
    <AskCard ask={ask} documentLink={false} initialThread={thread(ask)} variant="compact" />
  );
  const card = within(view.container);
  try {
    const approve = await card.findByRole("radio", { name: "Approve" });
    expect(approve).toHaveAttribute("data-ask-approval", "approve");
    const requestChanges = card.getByRole("radio", { name: "Request changes" });
    expect(requestChanges).toHaveAttribute("data-ask-approval", "request-changes");

    await user.click(requestChanges);
    const reason = card.getByLabelText("Reason (required)");
    expect(reason).toHaveFocus();
    await user.paste("Name the rollback path.");
    await user.keyboard("{Escape}");
    expect((requestChanges as HTMLInputElement).checked).toBe(false);
    expect(card.getByLabelText("Reason")).toHaveValue("Name the rollback path.");
  } finally {
    view.unmount();
  }
});

test("the diff cuts every decision block, so answers alone leave nothing changed", async () => {
  const user = userEvent.setup();
  const ask = approvalAsk(earlierApproval);
  const getArtifactVersion = mockVersions({
    11: decisionBlockFixture("approved.md"),
    12: decisionBlockFixture("answered.md"),
  });
  const { view } = renderCard(<AskCard ask={ask} initialThread={thread(ask)} />);
  const card = within(view.container);
  try {
    await user.click(card.getByRole("button", { name: "Show diff" }));
    const diff = await card.findByTestId("change-view");
    expect(diff.querySelector("ins, del")).toBeNull();
    expect(diff).toHaveTextContent("The platform team asked for a rollback path.");
    expect(diff.textContent).not.toContain("ask{");
  } finally {
    view.unmount();
    getArtifactVersion.mockRestore();
  }
});

test("the diff keeps a paragraph changed beside the decision blocks", async () => {
  const user = userEvent.setup();
  const ask = approvalAsk(earlierApproval);
  const getArtifactVersion = mockVersions({
    11: decisionBlockFixture("approved.md"),
    12: decisionBlockFixture("paragraph-changed.md"),
  });
  const { view } = renderCard(<AskCard ask={ask} initialThread={thread(ask)} />);
  const card = within(view.container);
  try {
    await user.click(card.getByRole("button", { name: "Show diff" }));
    const diff = await card.findByTestId("change-view");
    const changed = diff.querySelector('[data-change="changed"]');
    expect(changed?.querySelector("ins")?.textContent).toBe(" and a canary");
    expect(changed?.querySelector("del")).toBeNull();
    expect(changed?.textContent).toBe("The service ships behind a flag and a canary.");
    expect(diff.textContent).not.toContain("ask{");
  } finally {
    view.unmount();
    getArtifactVersion.mockRestore();
  }
});

test("a question closed under the reader keeps their unsent answer, out of the form", async () => {
  const user = userEvent.setup();
  const open = answeredAsk({ answer: null, state: "open" });
  const typed = "Only on a cold start, and only back to the last lease.";
  const { queryClient, view } = renderCard(<AskCard ask={open} initialThread={thread(open, [])} />);
  const card = within(view.container);
  try {
    await user.clear(card.getByLabelText("Your answer"));
    await user.click(card.getByLabelText("Your answer"));
    await user.paste(typed);

    // The server withdraws the question while that answer is still a draft.
    act(() => {
      queryClient.setQueryData<AskRead>(
        ["ask-thread", open.id],
        thread(
          {
            ...open,
            resolution: {
              actor: { id: "session-1", kind: "session" },
              at: "2026-10-08T03:00:00Z",
              kind: "retracted",
              reason: "A newer question supersedes this one.",
            },
            state: "resolved",
          },
          []
        )
      );
    });

    // The form is gone - an answer sent now is one the server refuses - and the text it held is
    // where the reader can still read and copy it.
    await waitFor(() =>
      expect(card.getByTestId("ask-resolution-badge").textContent).toBe("Retracted")
    );
    expect(card.queryByRole("button", { name: "Answer" })).not.toBeInTheDocument();
    expect(view.container.querySelector("[data-ask-answer]")).toBeNull();
    expect(card.getByTestId("ask-unsent-answer")).toHaveTextContent(typed);
  } finally {
    view.unmount();
  }
});

/** An ask resolved as `kind`, which a session did before anyone replied. */
function resolvedAsk(kind: string, overrides: Partial<Ask> = {}): Ask {
  return answeredAsk({
    answer: null,
    resolution: {
      actor: { id: "session-1", kind: "session" },
      at: "2026-10-08T03:00:00Z",
      kind,
      reason: "Settled elsewhere.",
    },
    state: "resolved",
    ...overrides,
  });
}

/** A `createReply` that records what it was asked to post, and answers the comment posted. */
function recordingReply(sent: [string, CreateCommentInput][]) {
  return async (issueKey: string, input: CreateCommentInput): Promise<Comment> => {
    sent.push([issueKey, input]);
    return { ...askReply(`reply-${sent.length}`, "2026-10-08T04:00:00Z"), body: input.body ?? "" };
  };
}

// The collapsed thread the Conversation and a decision block draw (`AskThreadDisclosure`) keeps
// its Write a reply under a resolved ask, as under an answered one, before anyone has replied
// too: however the question was settled, the conversation under it goes on.
test("a collapsed thread under a resolved ask offers Write a reply, and its reply goes on that ask", async () => {
  const user = userEvent.setup();
  for (const kind of ["resolved", "retracted", "declined", "withdrawn"]) {
    const resolved = resolvedAsk(kind);
    const sent: [string, CreateCommentInput][] = [];
    const { view } = renderCard(
      <AskCard
        ask={resolved}
        createReply={recordingReply(sent)}
        getAskThread={async () => thread(resolved)}
        initialThread={thread(resolved)}
        thread="collapsed"
      />
    );
    const card = within(view.container);
    try {
      await user.click(await card.findByRole("button", { name: "Write a reply" }));
      await user.click(card.getByRole("textbox", { name: "Reply" }));
      await user.paste(`After the ${kind} ask.`);
      await user.click(card.getByRole("button", { name: "Reply" }));
      await waitFor(() =>
        expect(sent).toEqual([["CORE-1", { ask_id: "ask-1", body: `After the ${kind} ask.` }]])
      );
    } finally {
      view.unmount();
    }
  }
});

// A reply being written belongs to the ask's thread, which a resolved ask keeps: the ask
// resolving under the reader, and a card mounted for it since, leave the draft where it was,
// and it is sent on that ask.
test("a reply draft outlives its ask resolving and its card remounting, and goes on that ask", async () => {
  const user = userEvent.setup();
  const answered = answeredAsk();
  const retracted = resolvedAsk("retracted", { answer: firstAnswer });
  const typed = "One more thing before this closes.";
  const sent: [string, CreateCommentInput][] = [];
  const first = renderCard(
    <AskCard ask={answered} createReply={recordingReply(sent)} initialThread={thread(answered)} />
  );
  try {
    const card = within(first.view.container);
    await user.click(await card.findByRole("textbox", { name: "Reply" }));
    await user.paste(typed);

    // Document settlement retracts the answered ask while the reply is still a draft.
    act(() => {
      first.queryClient.setQueryData<AskRead>(["ask-thread", answered.id], thread(retracted));
    });
    await waitFor(() =>
      expect(card.getByTestId("ask-resolution-badge").textContent).toBe("Retracted")
    );
    expect(card.getByRole("textbox", { name: "Reply" })).toHaveValue(typed);
  } finally {
    first.view.unmount();
  }

  // A card mounted for the retracted ask since, as a margin tab switched back mounts one.
  const again = renderCard(
    <AskCard
      ask={retracted}
      createReply={recordingReply(sent)}
      getAskThread={async () => thread(retracted)}
      initialThread={thread(retracted)}
    />
  );
  try {
    const card = within(again.view.container);
    expect(await card.findByRole("textbox", { name: "Reply" })).toHaveValue(typed);
    await user.click(card.getByRole("button", { name: "Reply" }));
    await waitFor(() => expect(sent).toEqual([["CORE-1", { ask_id: "ask-1", body: typed }]]));
  } finally {
    again.view.unmount();
  }
});

// A reply typed under a settled ask that then opens again (a withdrawn ask whose issue is
// reopened, a retracted question a document's settlement restores) has no composer left to sit
// in, since an open ask takes none: each thread shows the text read-only where its composer
// stood, so the reader can still copy it.
for (const surface of ["inline", "collapsed"] as const) {
  test(`a reply typed under a retracted ask that opens again stays on screen in the ${surface} thread`, async () => {
    const user = userEvent.setup();
    const retracted = resolvedAsk("retracted");
    const reopened = answeredAsk({ answer: null, state: "open" });
    const typed = "Before this is picked up again: the lease is the cause.";
    const card = (ask: Ask) => (
      <AskCard
        ask={ask}
        getAskThread={async () => thread(ask, [])}
        initialThread={thread(ask, [])}
        thread={surface}
      />
    );
    const { queryClient, view } = renderCard(card(retracted));
    const scope = within(view.container);
    try {
      if (surface === "collapsed") {
        await user.click(await scope.findByRole("button", { name: "Write a reply" }));
      }
      await user.click(await scope.findByRole("textbox", { name: "Reply" }));
      await user.paste(typed);

      // The ask opens again: its thread and the list the card is drawn from both read it open.
      act(() => {
        queryClient.setQueryData<AskRead>(["ask-thread", retracted.id], thread(reopened, []));
      });
      view.rerender(
        <MemoryRouter>
          <QueryClientProvider client={queryClient}>{card(reopened)}</QueryClientProvider>
        </MemoryRouter>
      );
      expect(await scope.findByLabelText("Your answer")).toBeInTheDocument();
      expect(await scope.findByTestId("ask-unsent-reply")).toHaveTextContent(typed);
      expect(scope.queryByRole("textbox", { name: "Reply" })).not.toBeInTheDocument();
    } finally {
      view.unmount();
    }
  });
}

// Handing a question to somebody else is a move on a question still waiting for an answer, so
// the control is part of an open card and of no record: an answered, a resolved or a withdrawn
// question waits on nobody.
test("the hand-on control is on an open question and on no closed one", async () => {
  const open = answeredAsk({ answer: null, state: "open" });
  const { view } = renderCard(<AskCard ask={open} initialThread={thread(open, [])} />);
  try {
    expect(
      await within(view.container).findByLabelText("Hand this question to")
    ).toBeInTheDocument();
  } finally {
    view.unmount();
  }

  const closedBy = (kind: "resolved" | "withdrawn"): Ask =>
    answeredAsk({
      answer: null,
      resolution: {
        actor: { id: "session-1", kind: "session" },
        at: "2026-10-08T03:00:00Z",
        kind,
        reason: "Settled elsewhere.",
      },
      state: "resolved",
    });
  for (const closed of [answeredAsk(), closedBy("resolved"), closedBy("withdrawn")]) {
    const card = renderCard(<AskCard ask={closed} initialThread={thread(closed)} />);
    try {
      // The record the card draws instead of the open question, which is what carries the
      // control: the open card's own article (`data-ask-card`) is not on screen at all.
      await waitFor(() => expect(card.view.getByTestId(`ask-${closed.id}`)).toBeInTheDocument());
      expect(card.view.container.querySelector("[data-ask-card]")).toBeNull();
      expect(
        within(card.view.container).queryByLabelText("Hand this question to")
      ).not.toBeInTheDocument();
    } finally {
      card.view.unmount();
    }
  }
});

// A hand-on moves whom the ask waits on, and the Inbox's Reply-thread card reads that person
// from the ask's own thread (`ThreadBody`, `askThreadQuery`), which is kept for the session and
// never goes stale of itself: without this read its picker would show the person from before
// the hand-on until the `ask.asked_of` event landed. Drawn through `ThreadBody`, because that
// is what hands the thread's own ask to the card: a card draws the `ask` it is given while the
// question is open (`useAskAnswerForm`), so one given a fixed ask could not show the write at
// all and the read alone would prove nothing.
test("a hand-on reads the ask's thread again, so a card reading that thread follows the write", async () => {
  const user = userEvent.setup();
  const open = answeredAsk({ answer: null, state: "open" });
  const handedOn: Ask = {
    ...open,
    asked_of: "bob",
    asked_of_at: "2026-10-08T04:00:00Z",
    asked_of_by: "alice",
    waiting_on_person: "bob",
  };
  let threadReads = 0;
  let handed = false;
  const getAsk = spyOn(api, "getAsk").mockImplementation(async () => {
    threadReads += 1;
    return thread(handed ? handedOn : open, []);
  });
  const handAskTo = spyOn(api, "handAskTo").mockImplementation(async () => {
    handed = true;
    return handedOn;
  });
  const listUsers = spyOn(api, "listUsers").mockResolvedValue([
    { login: "alice" },
    { login: "bob" },
  ]);
  // The card marks the thread read off its owner's unread set, which holds nothing here.
  const listMyReplies = spyOn(api, "listMyReplies").mockResolvedValue({
    as_of: "2026-10-08T00:00:00Z",
    limit: 200,
    offset: 0,
    rows: [],
    total: 0,
  });
  const { view } = renderCard(<ThreadBody thread={replyRow(open.id, { kind: "ask" })} />);
  const card = within(view.container);
  try {
    const control = await card.findByLabelText("Hand this question to");
    expect(control).toHaveAccessibleName("Hand this question to Asked of nobody");
    // The people are read only once the reader reaches for the control (`AskHandOn`).
    await user.click(control);
    // The list is portalled to the body, outside the card's own container.
    const bob = await within(document.body).findByRole("option", { name: "bob" });
    const before = threadReads;
    await user.click(bob);
    await waitFor(() => expect(handAskTo).toHaveBeenCalled());
    await waitFor(() => expect(threadReads).toBeGreaterThan(before));
    // What that read is for: the person the write moved the ask to is on the screen.
    await waitFor(() =>
      expect(card.getByLabelText("Hand this question to")).toHaveAccessibleName(
        "Hand this question to Asked of bob"
      )
    );
  } finally {
    view.unmount();
    getAsk.mockRestore();
    handAskTo.mockRestore();
    listMyReplies.mockRestore();
    listUsers.mockRestore();
  }
});

// INFRA-3135, R3: "Every list of options a reader picks from holds a text box that narrows the
// options." The card's "Hand this question to" is such a list: its options are the people who
// have signed in, which grow with the team. Opened, it holds a text box of its own, a word typed
// into it leaves only the people who match, and a pick hands the question on and puts the focus
// back on the control.
test("the hand-on list holds a text box that narrows its people, and a pick hands the question on", async () => {
  const user = userEvent.setup();
  const open = answeredAsk({
    answer: null,
    asked_of: "alice",
    state: "open",
    waiting_on_person: "alice",
  });
  const listUsers = spyOn(api, "listUsers").mockResolvedValue([
    { login: "alice" },
    { login: "bob" },
    { login: "carol" },
  ]);
  const handAskTo = spyOn(api, "handAskTo").mockResolvedValue({
    ...open,
    asked_of: "carol",
    waiting_on_person: "carol",
  });
  const getAsk = spyOn(api, "getAsk").mockResolvedValue(thread(open, []));
  const { view } = renderCard(<AskCard ask={open} initialThread={thread(open, [])} />);
  // The list is portalled to the body, outside the card's own container.
  const page = within(document.body);
  try {
    const before = new Set(textBoxes());
    await user.click(await within(view.container).findByLabelText("Hand this question to"));
    // The people are read once the reader reaches for the control.
    await waitFor(() =>
      expect(page.queryAllByRole("option", { name: /^bob\b/ }).length).toBeGreaterThan(0)
    );
    const own = textBoxes().filter((box) => !before.has(box));
    expect(own, "the hand-on list holds no text box of its own").toHaveLength(1);
    const list = listOf(own[0] as HTMLElement);
    expect(within(list).getByRole("option", { name: /^alice\b/ })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    await user.type(own[0] as HTMLElement, "car");
    await waitFor(() =>
      expect(
        within(list).queryAllByRole("option", { name: /^bob\b/ }),
        `"car" leaves bob in the hand-on list`
      ).toHaveLength(0)
    );
    expect(
      within(list).queryAllByRole("option", { name: /^alice\b/ }),
      `"car" leaves alice in the hand-on list`
    ).toHaveLength(0);
    await user.click(within(list).getByRole("option", { name: /^carol\b/ }));
    await waitFor(() => expect(handAskTo).toHaveBeenCalledWith("ask-1", "carol", keyedAttempt));
    expect(page.queryByRole("listbox")).not.toBeInTheDocument();
    expect(within(view.container).getByLabelText("Hand this question to")).toHaveFocus();
  } finally {
    view.unmount();
    getAsk.mockRestore();
    handAskTo.mockRestore();
    listUsers.mockRestore();
  }
});

// dispatch://INFRA-2160, R6: a hand-on is written the moment a person is picked, so the closed
// badge takes the list's keys, as a focused `<select>` does: an arrow or a letter only opens the
// list and moves its highlight, and never reaches the page's keymap, where `b`, `e` and `j` are
// the Inbox's own keys. Escape writes nothing and leaves the badge on the person it waits on,
// and only Enter hands the question on.
test("the hand-on badge's keys open its list and move the highlight, reach no page key, and only Enter hands the question on", async () => {
  const user = userEvent.setup();
  const open = answeredAsk({
    answer: null,
    asked_of: "alice",
    state: "open",
    waiting_on_person: "alice",
  });
  const listUsers = spyOn(api, "listUsers").mockResolvedValue([
    { login: "alice" },
    { login: "bob" },
    { login: "carol" },
  ]);
  const handAskTo = spyOn(api, "handAskTo").mockResolvedValue({
    ...open,
    asked_of: "bob",
    waiting_on_person: "bob",
  });
  const getAsk = spyOn(api, "getAsk").mockResolvedValue(thread(open, []));
  const { queryClient, view } = renderCard(<AskCard ask={open} initialThread={thread(open, [])} />);
  const pageKeys: string[] = [];
  const listen = (event: KeyboardEvent) => {
    if (!event.defaultPrevented) pageKeys.push(event.key);
  };
  window.addEventListener("keydown", listen);
  try {
    const control = await within(view.container).findByLabelText("Hand this question to");
    act(() => control.focus());
    // The people are read once the reader reaches for the control.
    await waitFor(() =>
      expect(queryClient.getQueryState(usersQuery().queryKey)?.status).toBe("success")
    );
    expect(listUsers).toHaveBeenCalledTimes(1);
    for (const [keys, to] of [
      ["{ArrowDown}", "bob"],
      ["{End}", "carol"],
      ["b", "bob"],
    ] as const) {
      await user.keyboard(keys);
      expect(
        highlightedRow(document.querySelector('[role="combobox"][aria-label="Search people"]')),
        `${keys} highlights ${to}`
      ).toBe(to);
      await user.keyboard("{Escape}");
      expect(within(document.body).queryByRole("listbox")).not.toBeInTheDocument();
      expect(control).toHaveFocus();
      expect(control).toHaveAccessibleName("Hand this question to Asked of alice");
    }
    expect(pageKeys, "no key on the closed badge reached the page").toEqual([]);
    expect(handAskTo).not.toHaveBeenCalled();

    await user.keyboard("b{Enter}");
    await waitFor(() => expect(handAskTo).toHaveBeenCalledWith("ask-1", "bob", keyedAttempt));
    expect(handAskTo).toHaveBeenCalledTimes(1);
  } finally {
    window.removeEventListener("keydown", listen);
    view.unmount();
    getAsk.mockRestore();
    handAskTo.mockRestore();
    listUsers.mockRestore();
  }
});

// INFRA-3135, round 12: the hand-on list passed no failure, so after a failed read of the people
// it offered the person the question waits on as if that were everyone, with nothing to say the
// people never came. It says so, as the Inbox's own people list does.
test("the hand-on list says the people could not be loaded when their read fails", async () => {
  const user = userEvent.setup();
  const open = answeredAsk({
    answer: null,
    asked_of: "alice",
    state: "open",
    waiting_on_person: "alice",
  });
  const listUsers = spyOn(api, "listUsers").mockRejectedValue(new Error("users unreachable"));
  const getAsk = spyOn(api, "getAsk").mockResolvedValue(thread(open, []));
  const { view } = renderCard(<AskCard ask={open} initialThread={thread(open, [])} />);
  // The list is portalled to the body, outside the card's own container.
  const page = within(document.body);
  try {
    await user.click(await within(view.container).findByLabelText("Hand this question to"));
    const list = await page.findByRole("listbox", { name: "Hand this question to options" });
    const panel = list.parentElement as HTMLElement;
    expect(await within(panel).findByText("Could not load the people list.")).toBeTruthy();
    expect(within(list).getByRole("option", { name: /^alice\b/ })).toHaveAttribute(
      "aria-selected",
      "true"
    );
  } finally {
    view.unmount();
    getAsk.mockRestore();
    listUsers.mockRestore();
  }
});

/** The thread's reply bodies, in the order the card draws them. */
function threadReplyBodies(thread: HTMLElement): (string | null | undefined)[] {
  return Array.from(
    thread.querySelectorAll("li"),
    (item) => item.querySelector(".dispatch-markdown")?.textContent
  );
}

/** Whether `first` comes before `second` in the document. */
function precedes(first: Element, second: Element): boolean {
  return (first.compareDocumentPosition(second) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0;
}

const THREAD_ORDER_KEY = userPreferenceStorageKey("alice", "inbox.thread-order");

// One choice, made in the Inbox's View menu, places every thread: its replies, the fold over the
// earlier ones, and the box the reader writes in - here the ask's own options and answer box,
// which sit where a reply box would.
for (const order of ["oldest", "newest"] as const) {
  test(`with ${order} first, the thread and the answer box read in that order`, async () => {
    window.localStorage.setItem(THREAD_ORDER_KEY, order);
    const open = answeredAsk({ answer: null, state: "open" });
    const replies = [
      askReply("reply-1", "2026-10-08T01:00:00Z"),
      askReply("reply-2", "2026-10-08T02:00:00Z"),
      askReply("reply-3", "2026-10-08T03:00:00Z"),
    ];
    const { view } = renderCard(
      <AskCard ask={open} initialThread={{ ...thread(open, []), replies }} />
    );
    try {
      // The card draws the thread in the browser's own order, which it reads once the signed-in
      // identity has landed, so the section it draws first is replaced by the ordered one.
      await waitFor(() =>
        expect(threadReplyBodies(view.getByTestId("thread-ask-1"))).toEqual(
          order === "oldest"
            ? ["Reply reply-2", "Reply reply-3"]
            : ["Reply reply-3", "Reply reply-2"]
        )
      );
      // The fold over the earlier reply sits furthest from the newest message, and the ask's
      // options and answer box beside it.
      const fold = view.getByRole("button", { name: "Show 1 earlier reply" });
      const newest = view.getByText("Reply reply-3");
      const answerBox = view.getByLabelText("Your answer");
      expect(precedes(fold, newest)).toBe(order === "oldest");
      expect(precedes(answerBox, view.getByTestId("thread-ask-1"))).toBe(order === "newest");
    } finally {
      view.unmount();
      window.localStorage.removeItem(THREAD_ORDER_KEY);
    }
  });
}

// The fold holds what the reader has already read, never a reply they have not: the unread set
// the mark reads (`useMarkThreadRead`) says where their reading stopped.
test("an unread reply is never folded away", async () => {
  const screen = installIntersectionObserver();
  const ask = answeredAsk();
  const replies = [1, 2, 3, 4].map((place) =>
    askReply(`reply-${place}`, `2026-10-08T0${place}:00:00Z`)
  );
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  seedUnread(queryClient, { issue: "CORE-1" }, []);
  // The reader read the thread through its first reply, so replies 2 to 4 are unread.
  queryClient.setQueryData(myRepliesOwnerQuery({ issue: "CORE-1" }).queryKey, {
    as_of: "2026-10-12T00:00:00Z",
    limit: 200,
    offset: 0,
    rows: [replyRow(ask.id, { kind: "ask", read_through: "2026-10-08T01:00:00Z" })],
    total: 1,
  });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <AskCard ask={ask} initialThread={{ ...thread(ask), replies }} markRead />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    const threadElement = await view.findByTestId("thread-ask-1");
    await waitFor(() =>
      expect(threadReplyBodies(threadElement)).toEqual([
        "Reply reply-2",
        "Reply reply-3",
        "Reply reply-4",
      ])
    );
    expect(view.getByRole("button", { name: "Show 1 earlier reply" })).toBeTruthy();
  } finally {
    view.unmount();
    screen.restore();
  }
});

// INFRA-869: a reply that is nothing but a reference shows the reference's card under it, as
// the margin's comments do.
test("a reply that is nothing but a reference shows its card", async () => {
  const getIssueSummary = spyOn(api, "getIssueSummary").mockResolvedValue({
    documents: [],
    key: "CORE-2",
    project: "CORE",
    status: "in_progress",
    title: "The other issue",
  });
  const ask = answeredAsk();
  const link = { ...askReply("reply-1", "2026-10-08T03:00:00Z"), body: "dispatch://CORE-2" };
  const { view } = renderCard(
    <AskCard ask={ask} initialThread={{ ...thread(ask), replies: [link] }} />
  );

  try {
    const reply = await waitFor(() => {
      const found = view.container.querySelector<HTMLElement>("[data-reply]");
      if (found === null) throw new Error("the reply has not rendered");
      return found;
    });
    const cards = await within(reply).findByRole("region", { name: "References" });
    expect(
      within(cards)
        .getByRole("link", { name: /The other issue/ })
        .getAttribute("href")
    ).toBe("/issues/CORE-2");
  } finally {
    getIssueSummary.mockRestore();
    view.unmount();
  }
});
