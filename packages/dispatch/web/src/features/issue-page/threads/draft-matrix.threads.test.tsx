/**
 * The draft matrix, Threads' opened record (dispatch://INFRA-5704, decision 506). A draft lives
 * at its box's own address: a reply box's under the entry it answers, the box at the foot's under
 * the record, an edit's under its comment. A box drawn for another entry or record does not
 * inherit it, and the box drawn again at its address shows it.
 *
 * | Move                                                      | reply box | foot box | edit  |
 * | --------------------------------------------------------- | --------- | -------- | ----- |
 * | A reply box goes for another entry's, then opens again    | typed     | -        | -     |
 * | The reader leaves a reply box, then opens it again        | typed     | -        | -     |
 * | The box at the foot and a reply box keep their own drafts | typed     | typed    | -     |
 * | Another record opens, then the record again               | typed     | typed    | typed |
 * | The window narrows below 1024 px, then widens             | -         | typed    | -     |
 * | New thread opens, then the record again                   | -         | typed    | -     |
 * | The thread New thread sent, then a page holds it          | -         | typed    | -     |
 * | An edit goes for a reply box, then opens again            | -         | -        | typed |
 * | The page reloaded, then back                              | -         | typed    | typed |
 *
 * `draft-matrix.threads.coverage.ts` ties the census to these cells and its numbered reasons.
 */
import { beforeAll, expect, spyOn, test } from "bun:test";
import type { CommentThreadPage, MessageThreadPage } from "@legion/contracts";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";

import {
  BODY,
  endAsAReload,
  newQueryClient,
  signedInAsReader,
} from "../../../__tests__/draft-matrix-fixture";
import { plannerAgent } from "../../../__tests__/search-fixture";
import { api } from "../../../api/client";
import type { Message } from "../../../api/types";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import { CONTROL_LABELS, THREADS_WORDS } from "../page-words";
import asksThreadBody from "./fixtures/asks-thread.json";
import commentsInBody from "./fixtures/comments-in.json";
import messageCreatedBody from "./fixtures/message-created.json";
import messagesInBody from "./fixtures/messages-in.json";
import { answerThreads, drawThreads, KEY, stubWindow, threadsAt } from "./threads-view-fixture";

const HOST = "Threads record";
const COMMENTS = (commentsInBody as unknown as CommentThreadPage).threads ?? [];

/** The reader's remark and the reader's comment on the spec's text: two records whose roots
 *  are the reader's own, each with replies. */
const REMARK = COMMENTS.find((row) => row.comment.body.startsWith("The list should name"));
const TEXT = COMMENTS.find((row) => row.comment.body.startsWith("Is a tie broken"));
const REMARK_ID = REMARK?.comment.id ?? "";
const TEXT_ID = TEXT?.comment.id ?? "";
const REPLY_ID = REMARK?.replies?.[0]?.id ?? "";
/** The settled question with replies. */
const SETTLED = asksThreadBody.threads.find((row) => row.question.startsWith("Does the list keep"));
const SETTLED_ID = SETTLED?.id ?? "";
const SETTLED_REPLY_ID = SETTLED?.thread?.replies?.[0]?.id ?? "";

const entry = (id: string) => {
  const found = document.querySelector<HTMLElement>(`[data-thread-entry="${id}"]`);
  if (found === null) throw new Error(`The record draws no entry ${id}.`);
  return found;
};
const boxOf = (which: string) =>
  document.querySelector<HTMLElement>(`[data-thread-box="${which}"]`) ?? undefined;
const field = (which: string) =>
  within(boxOf(which) ?? document.body).getByRole<HTMLTextAreaElement>("textbox", {
    name: CONTROL_LABELS.reply,
  });
const editField = (id: string) =>
  within(entry(id)).getByRole<HTMLTextAreaElement>("textbox", { name: "Edit comment" });

async function opened(id: string): Promise<void> {
  await waitFor(() =>
    expect(document.querySelector("[data-thread-record]")).toHaveAttribute("data-thread-record", id)
  );
  await waitFor(() => expect(boxOf("foot") ?? boxOf(id)).toBeDefined());
}

async function press(user: UserEvent, id: string, name: string): Promise<void> {
  await user.click(within(entry(id)).getByRole("button", { name }));
}

async function write(user: UserEvent, target: HTMLTextAreaElement, words = BODY): Promise<void> {
  await user.click(target);
  await user.paste(words);
}

beforeAll(async () => {
  await warmMarkdownRenderer();
});

for (const kind of ["comment", "ask"] as const) {
  test(`${HOST} · a reply box goes for another entry's, then opens again · ${kind} · typed`, async () => {
    const window = stubWindow(1440);
    answerThreads();
    const user = userEvent.setup();
    const [record, first, second] =
      kind === "comment"
        ? [REMARK_ID, REMARK_ID, REPLY_ID]
        : [SETTLED_ID, SETTLED_ID, SETTLED_REPLY_ID];
    const drawn = drawThreads(threadsAt(record));
    try {
      await opened(record);
      await press(user, first, CONTROL_LABELS.reply);
      await write(user, field(first));
      await press(user, second, CONTROL_LABELS.reply);
      expect(boxOf(first)).toBeUndefined();
      expect(field(second).value).toBe("");
      await press(user, first, CONTROL_LABELS.reply);
      expect(field(first).value).toBe(BODY);
    } finally {
      drawn.done();
      window.restore();
    }
  });
}

test(`${HOST} · the reader leaves a reply box, then opens it again · typed`, async () => {
  const window = stubWindow(1440);
  answerThreads();
  const user = userEvent.setup();
  const drawn = drawThreads(threadsAt(REMARK_ID));
  try {
    await opened(REMARK_ID);
    await press(user, REPLY_ID, CONTROL_LABELS.reply);
    await write(user, field(REPLY_ID));
    await user.click(
      within(boxOf(REPLY_ID) as HTMLElement).getByRole("button", { name: "Cancel reply" })
    );
    expect(boxOf(REPLY_ID)).toBeUndefined();
    await press(user, REPLY_ID, CONTROL_LABELS.reply);
    expect(field(REPLY_ID).value).toBe(BODY);
  } finally {
    drawn.done();
    window.restore();
  }
});

test(`${HOST} · the box at the foot and a reply box keep their own drafts · typed`, async () => {
  const window = stubWindow(1440);
  answerThreads();
  const user = userEvent.setup();
  const drawn = drawThreads(threadsAt(REMARK_ID));
  try {
    await opened(REMARK_ID);
    await write(user, field("foot"));
    await press(user, REMARK_ID, CONTROL_LABELS.reply);
    expect(field(REMARK_ID).value, "the root's reply box took the foot's draft").toBe("");
    await write(user, field(REMARK_ID), "Another draft");
    expect(field("foot").value).toBe(BODY);
  } finally {
    drawn.done();
    window.restore();
  }
});

for (const box of ["foot", "reply", "edit"] as const) {
  test(`${HOST} · another record opens, then the record again · ${box} · typed`, async () => {
    const window = stubWindow(1440);
    answerThreads();
    const user = userEvent.setup();
    const drawn = drawThreads(threadsAt(REMARK_ID));
    const shown = (): HTMLTextAreaElement =>
      box === "foot" ? field("foot") : box === "reply" ? field(REPLY_ID) : editField(REMARK_ID);
    const open = async (): Promise<void> => {
      if (box === "reply") await press(user, REPLY_ID, CONTROL_LABELS.reply);
      if (box === "edit") await press(user, REMARK_ID, CONTROL_LABELS.edit);
    };
    try {
      await opened(REMARK_ID);
      await open();
      const before = shown().value;
      await write(user, shown());
      drawn.go(threadsAt(TEXT_ID));
      await opened(TEXT_ID);
      expect(field("foot").value, "another record's box took the draft").toBe("");
      drawn.go(threadsAt(REMARK_ID));
      await opened(REMARK_ID);
      await open();
      expect(shown().value).toBe(`${before}${BODY}`);
    } finally {
      drawn.done();
      window.restore();
    }
  });
}

test(`${HOST} · the window narrows below 1024 px, then widens · typed`, async () => {
  const window = stubWindow(1440);
  answerThreads();
  const user = userEvent.setup();
  const drawn = drawThreads(threadsAt(REMARK_ID));
  try {
    await opened(REMARK_ID);
    await write(user, field("foot"));
    window.resize(390);
    expect(screen.getByRole("button", { name: THREADS_WORDS.back_to_list })).toBeTruthy();
    expect(field("foot").value).toBe(BODY);
    window.resize(1440);
    expect(document.querySelector("[data-thread-list]")).not.toBeNull();
    expect(field("foot").value).toBe(BODY);
  } finally {
    drawn.done();
    window.restore();
  }
});

test(`${HOST} · New thread opens, then the record again · typed`, async () => {
  const window = stubWindow(1440);
  answerThreads();
  const user = userEvent.setup();
  const drawn = drawThreads(threadsAt(REMARK_ID));
  try {
    await opened(REMARK_ID);
    await write(user, field("foot"));
    await user.click(screen.getByRole("button", { name: THREADS_WORDS.new_thread }));
    await waitFor(() => expect(document.querySelector("[data-thread-record]")).toBeNull());
    drawn.go(threadsAt(REMARK_ID));
    await opened(REMARK_ID);
    expect(field("foot").value).toBe(BODY);
  } finally {
    drawn.done();
    window.restore();
  }
});

test(`${HOST} · the thread New thread sent, then a page holds it · typed`, async () => {
  const window = stubWindow(1440);
  const created = messageCreatedBody as unknown as Message;
  const page = messagesInBody as unknown as MessageThreadPage;
  // Once the case says the page holds the new thread, the messages' first page answers it first,
  // as the server orders a page by last activity.
  let held = false;
  answerThreads({
    messages: () =>
      held
        ? {
            ...page,
            threads: [
              { last_activity_at: created.created_at, message: created, replies: [] },
              ...(page.threads ?? []),
            ],
          }
        : page,
  });
  const create = spyOn(api, "createMessage").mockResolvedValue(created as never);
  const user = userEvent.setup();
  const drawn = drawThreads(threadsAt());
  const agents = spyOn(api, "listAgents").mockResolvedValue([plannerAgent()]);
  try {
    await user.click(await screen.findByRole("button", { name: THREADS_WORDS.new_thread }));
    await user.click(await screen.findByRole("option", { name: /^planner\b/ }));
    await write(
      user,
      screen.getByRole("textbox", { name: THREADS_WORDS.message_box }),
      "To the planner."
    );
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.send }));
    await opened(created.id);
    expect(
      document.querySelector(`[data-thread-row="${created.id}"]`),
      "a page held the new thread before the case said so"
    ).toBeNull();
    await write(user, field("foot"));
    held = true;
    await act(async () => {
      await drawn.client.invalidateQueries({ queryKey: ["issue-messages", KEY] });
    });
    await waitFor(() =>
      expect(document.querySelector(`[data-thread-row="${created.id}"]`)).not.toBeNull()
    );
    expect(field("foot").value).toBe(BODY);
  } finally {
    create.mockRestore();
    agents.mockRestore();
    drawn.done();
    window.restore();
  }
});

test(`${HOST} · an edit goes for a reply box, then opens again · typed`, async () => {
  const window = stubWindow(1440);
  answerThreads();
  const user = userEvent.setup();
  const drawn = drawThreads(threadsAt(REMARK_ID));
  try {
    await opened(REMARK_ID);
    await press(user, REMARK_ID, CONTROL_LABELS.edit);
    const before = editField(REMARK_ID).value;
    await write(user, editField(REMARK_ID));
    await press(user, REPLY_ID, CONTROL_LABELS.reply);
    expect(within(entry(REMARK_ID)).queryByRole("textbox")).toBeNull();
    await press(user, REMARK_ID, CONTROL_LABELS.edit);
    expect(editField(REMARK_ID).value).toBe(`${before}${BODY}`);
  } finally {
    drawn.done();
    window.restore();
  }
});

for (const box of ["reply", "edit"] as const) {
  test(`${HOST} · the page reloaded, then back · ${box} · typed`, async () => {
    const window = stubWindow(1440);
    answerThreads();
    const user = userEvent.setup();
    let drawn = drawThreads(threadsAt(REMARK_ID));
    try {
      await opened(REMARK_ID);
      if (box === "edit") await press(user, REMARK_ID, CONTROL_LABELS.edit);
      const shown = () => (box === "reply" ? field("foot") : editField(REMARK_ID));
      const before = shown().value;
      await write(user, shown());
      endAsAReload(() => drawn.done());
      drawn = drawThreads(threadsAt(REMARK_ID), { client: signedInAsReader(newQueryClient()) });
      await opened(REMARK_ID);
      if (box === "edit") await press(user, REMARK_ID, CONTROL_LABELS.edit);
      expect(shown().value).toBe(`${before}${BODY}`);
    } finally {
      drawn.done();
      window.restore();
    }
  });
}
