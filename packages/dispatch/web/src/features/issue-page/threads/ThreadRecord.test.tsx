import { afterEach, beforeAll, describe, expect, jest, spyOn, test } from "bun:test";
import type { CommentThreadPage, MessageThreadPage } from "@legion/contracts";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { BODY, newQueryClient, REFUSAL } from "../../../__tests__/draft-matrix-fixture";
import { jump, realClock } from "../../../__tests__/fake-clock";
import {
  dwellOn,
  type IntersectionControl,
  installIntersectionObserver,
} from "../../../__tests__/intersection-observer";
import { keyedAttempt } from "../../../__tests__/keyed-attempt";
import { answerThreadReads } from "../../../__tests__/send-answers";
import { api } from "../../../api/client";
import type { Message } from "../../../api/types";
import { compareTimestamps } from "../../../lib/timestamps";
import {
  borderDefault,
  calloutSuccessBg,
  inputClasses,
  pageHairBorder,
  pageInkText,
  pageLineBorder,
  pageMutedText,
  pagePaneBg,
  pagePaperBg,
} from "../../../theme/classes";
import { ASK_DECLINE_WORDS } from "../../ask-body/ask-body-words";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import { ANSWER_LINES, CONTROL_LABELS, THREADS_WORDS } from "../page-words";
import { publishedNames } from "../projection-fixture";
import asksThreadBody from "./fixtures/asks-thread.json";
import commentsInBody from "./fixtures/comments-in.json";
import commentsInBeforeBody from "./fixtures/comments-in-before.json";
import messageCreatedBody from "./fixtures/message-created.json";
import messagesInBody from "./fixtures/messages-in.json";
import { commentThreadsQuery } from "./thread-reads";
import { answerThreads, drawThreads, KEY, stubWindow, threadsAt } from "./threads-view-fixture";

/**
 * The opened record of Threads (dispatch://INFRA-5704/spec#b-r-open, R9) over the server's answers
 * captured at the stack base (`fixtures/README.md`): the read mark, the boxes and the send (R11),
 * the renderer its entries are drawn through, and the link of a record on text to its node (R8).
 */

const COMMENTS = (commentsInBody as unknown as CommentThreadPage).threads ?? [];
const MESSAGES = (messagesInBody as unknown as MessageThreadPage).threads ?? [];

/** The thread of a page whose root's words begin with `words`. */
function commentThread(words: string) {
  const found = COMMENTS.find((row) => row.comment.body.startsWith(words));
  if (found === undefined) throw new Error(`No comment page of the fixtures holds "${words}".`);
  return found;
}

/** The reader's remark, with the planner's reply and the Legion daemon's: a thread of the
 *  reader's unread set (`replies-limit-200.json`). */
const REMARK = commentThread("The list should name the daemon");
/** The reader's comment on the spec's text, also of the unread set. */
const TEXT = commentThread("Is a tie broken by the id?");
/** The settled question with replies, of no row of the unread set. */
const SETTLED = asksThreadBody.threads.find((row) => row.question.startsWith("Does the list keep"));
/** The question still open, with replies, which the reader may answer or decline. */
const OPEN_ASK = asksThreadBody.threads.find((row) =>
  row.question.startsWith("Should Waiting on me")
);
/** The reader's message to the planner's role, with the planner's replies. */
const FIRST_MESSAGE = MESSAGES.find((row) => row.message.body.startsWith("Can the list show"));

/** The newest time of a thread's root and its replies, which its read mark reads through. */
function newestOf(thread: (typeof COMMENTS)[number]): string {
  return [thread.comment, ...(thread.replies ?? [])]
    .map((post) => post.created_at)
    .reduce((newest, at) => (compareTimestamps(at, newest) > 0 ? at : newest));
}

const opened = () => document.querySelector("[data-thread-record]");
const entry = (id: string) =>
  document.querySelector<HTMLElement>(`[data-thread-entry="${id}"]`) ?? undefined;
const box = (which: string) =>
  document.querySelector<HTMLElement>(`[data-thread-box="${which}"]`) ?? undefined;
const replyField = (which: string) =>
  within(box(which) ?? document.body).getByRole<HTMLTextAreaElement>("textbox", {
    name: CONTROL_LABELS.reply,
  });

/** The read mark's watched element of the opened record, once the mark watches it. */
async function watchedFoot(control: IntersectionControl): Promise<Element> {
  return waitFor(() => {
    const foot = opened()?.querySelector("[data-thread-read]");
    expect(foot, "the opened record draws no read mark's foot").toBeTruthy();
    expect(control.observed()).toContain(foot as Element);
    return foot as Element;
  });
}

/** Restores the real clock a case installed the fake one for, where it still stands: a case that
 *  failed before it installed it leaves the real one. */
function restoreClock(): void {
  try {
    realClock();
  } catch {
    jest.useRealTimers();
  }
}

beforeAll(async () => {
  await warmMarkdownRenderer();
});

afterEach(() => {
  jest.useRealTimers();
});

describe("the read mark (R9)", () => {
  test("a record whose newest entry stood on screen under a second sends no mark, and once it stood a second one, with the thread's id and the newest entry's time", async () => {
    const window = stubWindow(1440);
    const control = installIntersectionObserver();
    const mark = answerThreadReads().mockResolvedValue(undefined);
    answerThreads();
    const drawn = drawThreads(threadsAt(REMARK.comment.id));
    try {
      const foot = await watchedFoot(control);
      jest.useFakeTimers();
      act(() => control.show(foot));
      act(() => {
        jest.advanceTimersByTime(999);
      });
      expect(mark).not.toHaveBeenCalled();
      await jump(1);
      expect(mark.mock.calls).toEqual([
        [REMARK.comment.id, { read_through: newestOf(REMARK) }, keyedAttempt],
      ]);
    } finally {
      restoreClock();
      drawn.done();
      mark.mockRestore();
      control.restore();
      window.restore();
    }
  });

  test("a row drawn and not opened sends no mark", async () => {
    const window = stubWindow(1440);
    const control = installIntersectionObserver();
    const mark = answerThreadReads().mockResolvedValue(undefined);
    answerThreads();
    const drawn = drawThreads(threadsAt());
    try {
      await waitFor(() => expect(document.querySelectorAll("[data-thread-row]").length).toBe(20));
      jest.useFakeTimers();
      for (const element of document.querySelectorAll("[data-thread-row]")) {
        act(() => control.show(element));
      }
      await jump(2_000);
      expect(mark).not.toHaveBeenCalled();
      expect(control.observed(), "a row drawn is watched for a read mark").toEqual([]);
    } finally {
      restoreClock();
      drawn.done();
      mark.mockRestore();
      control.restore();
      window.restore();
    }
  });

  test("a second record opened sends its own mark and reads the reader's unread threads no second time", async () => {
    const window = stubWindow(1440);
    const control = installIntersectionObserver();
    const mark = answerThreadReads().mockResolvedValue(undefined);
    const sent = answerThreads();
    const drawn = drawThreads(threadsAt(REMARK.comment.id));
    try {
      const first = await watchedFoot(control);
      expect(sent.limited, "opening a record read the unread threads other than once").toBe(1);
      jest.useFakeTimers();
      await dwellOn(control, first);
      realClock();
      // The mark that landed reads the unread threads again, as `useMarkThreadRead` asks. That
      // read is the mark's, after its PUT, and no opening's.
      await waitFor(() => expect(drawn.client.isFetching()).toBe(0));
      const read = sent.limited;
      drawn.go(threadsAt(TEXT.comment.id));
      const second = await waitFor(() => {
        expect(opened()).toHaveAttribute("data-thread-record", TEXT.comment.id);
        return watchedFoot(control);
      });
      await waitFor(() => expect(drawn.client.isFetching()).toBe(0));
      expect(sent.limited, "opening the second record read the unread threads again").toBe(read);
      jest.useFakeTimers();
      await dwellOn(control, second);
      expect(mark.mock.calls.map((call) => call.slice(0, 2))).toEqual([
        [REMARK.comment.id, { read_through: newestOf(REMARK) }],
        [TEXT.comment.id, { read_through: newestOf(TEXT) }],
      ]);
    } finally {
      restoreClock();
      drawn.done();
      mark.mockRestore();
      control.restore();
      window.restore();
    }
  });

  test("a record of a thread outside the reader's unread set sends no mark", async () => {
    const window = stubWindow(1440);
    const control = installIntersectionObserver();
    const mark = answerThreadReads().mockResolvedValue(undefined);
    const sent = answerThreads();
    const drawn = drawThreads(threadsAt(SETTLED?.id));
    try {
      await waitFor(() => expect(opened()).toHaveAttribute("data-thread-record", SETTLED?.id));
      await waitFor(() => expect(sent.limited).toBe(1));
      const foot = opened()?.querySelector("[data-thread-read]");
      expect(foot).toBeTruthy();
      expect(control.observed()).not.toContain(foot as Element);
      jest.useFakeTimers();
      await dwellOn(control, foot as Element);
      expect(mark).not.toHaveBeenCalled();
    } finally {
      restoreClock();
      drawn.done();
      mark.mockRestore();
      control.restore();
      window.restore();
    }
  });

  test("a new entry while the record is open sends a mark through its time once it dwells", async () => {
    const window = stubWindow(1440);
    const control = installIntersectionObserver();
    const mark = answerThreadReads().mockResolvedValue(undefined);
    answerThreads();
    const drawn = drawThreads(threadsAt(REMARK.comment.id));
    const later = "2026-11-26T12:00:00Z";
    try {
      const first = await watchedFoot(control);
      jest.useFakeTimers();
      await dwellOn(control, first);
      realClock();
      const reply = {
        ...(REMARK.replies?.[0] ?? REMARK.comment),
        created_at: later,
        id: "new-reply",
      };
      act(() => {
        drawn.client.setQueryData(commentThreadsQuery(KEY).queryKey, (data) =>
          data === undefined
            ? data
            : {
                ...data,
                pages: data.pages.map((page) => ({
                  ...page,
                  threads: (page.threads ?? []).map((row) =>
                    row.comment.id === REMARK.comment.id
                      ? {
                          ...row,
                          last_activity_at: later,
                          replies: [...(row.replies ?? []), reply],
                        }
                      : row
                  ),
                })),
              }
        );
      });
      await waitFor(() => expect(entry("new-reply")).toBeDefined());
      const foot = await watchedFoot(control);
      expect(entry("new-reply")?.contains(foot), "the mark does not watch the new entry").toBe(
        true
      );
      jest.useFakeTimers();
      await dwellOn(control, foot);
      expect(mark.mock.calls.map((call) => call.slice(0, 2))).toEqual([
        [REMARK.comment.id, { read_through: newestOf(REMARK) }],
        [REMARK.comment.id, { read_through: later }],
      ]);
    } finally {
      restoreClock();
      drawn.done();
      mark.mockRestore();
      control.restore();
      window.restore();
    }
  });

  test("opening a record for a non-human principal sends no unread-threads read", async () => {
    const window = stubWindow(1440);
    const control = installIntersectionObserver();
    const mark = answerThreadReads().mockResolvedValue(undefined);
    const sent = answerThreads();
    const client = newQueryClient();
    client.setQueryData(["whoami"], {
      event_head: "0",
      kind: "session",
      login: "threads-planner",
    });
    const drawn = drawThreads(threadsAt(REMARK.comment.id), { client });
    try {
      await waitFor(() =>
        expect(opened()).toHaveAttribute("data-thread-record", REMARK.comment.id)
      );
      await waitFor(() => expect(drawn.client.isFetching()).toBe(0));
      expect(
        sent.limited,
        "opening a record for a non-human principal read /me/replies?limit=200"
      ).toBe(0);
    } finally {
      drawn.done();
      mark.mockRestore();
      control.restore();
      window.restore();
    }
  });
});

/** The issue the remark is on, which its comment pages are keyed by. */
const KEY_OF_REMARK = REMARK.comment.issue_key;

describe("the boxes and the send (R9, R11)", () => {
  test("a reply control pressed while another reply box is open closes the first, whose draft comes back with it", async () => {
    const window = stubWindow(1440);
    answerThreads();
    const user = userEvent.setup();
    const drawn = drawThreads(threadsAt(REMARK.comment.id));
    const [planner] = REMARK.replies ?? [];
    try {
      await waitFor(() => expect(entry(REMARK.comment.id)).toBeDefined());
      const replyOf = (id: string) =>
        within(entry(id) as HTMLElement).getByRole("button", { name: CONTROL_LABELS.reply });
      await user.click(replyOf(REMARK.comment.id));
      await user.click(replyField(REMARK.comment.id));
      await user.paste(BODY);
      await user.click(replyOf(planner?.id ?? ""));
      expect(box(REMARK.comment.id), "two reply boxes stand open").toBeUndefined();
      expect(replyField(planner?.id ?? "").value, "a box took another box's draft").toBe("");
      await user.click(replyOf(REMARK.comment.id));
      expect(replyField(REMARK.comment.id).value).toBe(BODY);
      expect(box(planner?.id ?? "")).toBeUndefined();
    } finally {
      drawn.done();
      window.restore();
    }
  });

  test("the reader leaves a reply box with Escape, and it goes", async () => {
    const window = stubWindow(1440);
    answerThreads();
    const user = userEvent.setup();
    const drawn = drawThreads(threadsAt(REMARK.comment.id));
    try {
      await waitFor(() => expect(entry(REMARK.comment.id)).toBeDefined());
      await user.click(
        within(entry(REMARK.comment.id) as HTMLElement).getByRole("button", {
          name: CONTROL_LABELS.reply,
        })
      );
      await user.click(replyField(REMARK.comment.id));
      await user.keyboard("{Escape}");
      expect(box(REMARK.comment.id), "the box stays once the reader left it").toBeUndefined();
      expect(box("foot"), "the box at the foot went with it").toBeDefined();
    } finally {
      drawn.done();
      window.restore();
    }
  });

  test("a message box sends to the thread's target in its delivery, holds the words while the send is out, and goes once it is sent", async () => {
    const window = stubWindow(1440);
    answerThreads();
    const user = userEvent.setup();
    const answered = Promise.withResolvers<Message>();
    const create = spyOn(api, "createMessage").mockReturnValue(answered.promise as never);
    const root = FIRST_MESSAGE?.message;
    const drawn = drawThreads(threadsAt(root?.id));
    try {
      await waitFor(() => expect(entry(root?.id ?? "")).toBeDefined());
      await user.click(
        within(entry(root?.id ?? "") as HTMLElement).getByRole("button", {
          name: CONTROL_LABELS.reply,
        })
      );
      const field = replyField(root?.id ?? "");
      expect(field, "the reply control's box takes no caret").toHaveFocus();
      await user.type(field, "Under the root.");
      await user.click(
        within(box(root?.id ?? "") as HTMLElement).getByRole("button", {
          name: CONTROL_LABELS.send,
        })
      );
      expect(create.mock.calls.map((call) => call.slice(0, 2)) as unknown[]).toEqual([
        [
          KEY_OF_REMARK,
          {
            body: "Under the root.",
            delivery: root?.deliveries?.[0]?.delivery,
            in_reply_to: root?.id,
            target: root?.target,
          },
        ],
      ]);
      await waitFor(() => expect(replyField(root?.id ?? "")).toBeDisabled());
      expect(
        within(box(root?.id ?? "") as HTMLElement).getByText(ANSWER_LINES.sending)
      ).toBeTruthy();
      expect(replyField(root?.id ?? "").value).toBe("Under the root.");
      await act(async () => answered.resolve(messageCreatedBody as unknown as Message));
      await waitFor(() => expect(box(root?.id ?? ""), "the box stays once sent").toBeUndefined());
    } finally {
      answered.resolve(messageCreatedBody as unknown as Message);
      create.mockRestore();
      drawn.done();
      window.restore();
    }
  });

  test("a refused message keeps its words and offers Retry, which sends them again", async () => {
    const window = stubWindow(1440);
    answerThreads();
    const user = userEvent.setup();
    const create = spyOn(api, "createMessage").mockRejectedValue(REFUSAL);
    const root = FIRST_MESSAGE?.message;
    const drawn = drawThreads(threadsAt(root?.id));
    try {
      await waitFor(() => expect(box("foot")).toBeDefined());
      await user.type(replyField("foot"), "At the foot.");
      await user.click(
        within(box("foot") as HTMLElement).getByRole("button", { name: CONTROL_LABELS.send })
      );
      await within(box("foot") as HTMLElement).findByText(REFUSAL.message);
      expect(replyField("foot").value, "the refusal took the reader's words").toBe("At the foot.");
      await user.click(
        within(box("foot") as HTMLElement).getByRole("button", { name: CONTROL_LABELS.retry })
      );
      await waitFor(() => expect(create).toHaveBeenCalledTimes(2));
      expect(create.mock.calls[1]?.[1]).toEqual(create.mock.calls[0]?.[1]);
    } finally {
      create.mockRestore();
      drawn.done();
      window.restore();
    }
  });

  test("a box that writes a message offers nothing on @ and keeps the words as typed", async () => {
    const window = stubWindow(1440);
    answerThreads();
    const user = userEvent.setup();
    const drawn = drawThreads(threadsAt(FIRST_MESSAGE?.message.id));
    try {
      await waitFor(() => expect(box("foot")).toBeDefined());
      await user.type(replyField("foot"), "@plan");
      expect(screen.queryByRole("listbox"), "a message box offers parties on @").toBeNull();
      expect(replyField("foot").value).toBe("@plan");
      expect(replyField("foot")).not.toHaveAttribute("aria-expanded");
    } finally {
      drawn.done();
      window.restore();
    }
  });

  test("a comment whose author login differs from the viewer's only in case carries Edit", async () => {
    const window = stubWindow(1440);
    answerThreads({
      comments: () => ({
        ...commentsInBody,
        threads: COMMENTS.map((row) =>
          row.comment.id === REMARK.comment.id
            ? {
                ...row,
                comment: {
                  ...row.comment,
                  author: { ...row.comment.author, id: row.comment.author.id.toUpperCase() },
                },
              }
            : row
        ),
      }),
    });
    const drawn = drawThreads(threadsAt(REMARK.comment.id));
    try {
      await waitFor(() => expect(entry(REMARK.comment.id)).toBeDefined());
      expect(
        within(entry(REMARK.comment.id) as HTMLElement).getByRole("button", {
          name: CONTROL_LABELS.edit,
        }),
        "a comment whose author differs only in case omitted Edit"
      ).toBeTruthy();
    } finally {
      drawn.done();
      window.restore();
    }
  });

  test("a reply under an ask's reply and an edit of an ask's reply both refresh the ask pages", async () => {
    const window = stubWindow(1440);
    const sent = answerThreads();
    const user = userEvent.setup();
    const plannerReply = SETTLED?.thread.replies[0];
    const readerReply = SETTLED?.thread.replies[1];
    const create = spyOn(api, "createComment").mockResolvedValue({
      ...(readerReply ?? {}),
      body: "Under the ask's reply.",
      id: "ask-sub-reply",
    } as never);
    const edit = spyOn(api, "editComment").mockResolvedValue({
      ...(readerReply ?? {}),
      body: "Thank you very much.",
    } as never);
    const drawn = drawThreads(threadsAt(SETTLED?.id));
    try {
      await waitFor(() => expect(entry(plannerReply?.id ?? "")).toBeDefined());
      await waitFor(() => expect(drawn.client.isFetching()).toBe(0));
      const asksBeforeReply = sent.asks;
      await user.click(
        within(entry(plannerReply?.id ?? "") as HTMLElement).getByRole("button", {
          name: CONTROL_LABELS.reply,
        })
      );
      await user.click(replyField(plannerReply?.id ?? ""));
      await user.paste("Under the ask's reply.");
      await user.click(
        within(box(plannerReply?.id ?? "") as HTMLElement).getByRole("button", {
          name: CONTROL_LABELS.send,
        })
      );
      await waitFor(() =>
        expect(
          sent.asks,
          "replying under an ask's reply did not read the ask pages again"
        ).toBeGreaterThan(asksBeforeReply)
      );
      await waitFor(() => expect(drawn.client.isFetching()).toBe(0));
      const asksBeforeEdit = sent.asks;
      await user.click(
        within(entry(readerReply?.id ?? "") as HTMLElement).getByRole("button", {
          name: CONTROL_LABELS.edit,
        })
      );
      const field = within(
        entry(readerReply?.id ?? "") as HTMLElement
      ).getByRole<HTMLTextAreaElement>("textbox", { name: "Edit comment" });
      await user.click(field);
      await user.paste(" Very much.");
      await user.click(
        within(entry(readerReply?.id ?? "") as HTMLElement).getByRole("button", {
          name: "Save",
        })
      );
      await waitFor(() =>
        expect(
          sent.asks,
          "editing a comment inside an ask thread did not read the ask pages again"
        ).toBeGreaterThan(asksBeforeEdit)
      );
    } finally {
      create.mockRestore();
      edit.mockRestore();
      drawn.done();
      window.restore();
    }
  });
});

describe("the renderer of an entry (R9)", () => {
  test("an opened record draws an entry through the outline's renderer, a reference in it read as its target's name", async () => {
    const window = stubWindow(1440);
    const cited = "dispatch://CORE-2/spec#b-r1";
    const name = "the queue's drain";
    const references = spyOn(api, "getReferences").mockImplementation(() => new Promise(() => {}));
    const [planner] = REMARK.replies ?? [];
    answerThreads({
      comments: () => ({
        ...commentsInBody,
        threads: COMMENTS.map((row) =>
          row.comment.id === REMARK.comment.id
            ? {
                ...row,
                replies: (row.replies ?? []).map((reply) =>
                  reply.id === planner?.id
                    ? { ...reply, body: `Read ${cited} **first**.`, body_tree: undefined }
                    : reply
                ),
              }
            : row
        ),
      }),
    });
    const drawn = drawThreads(threadsAt(REMARK.comment.id), {
      published: publishedNames(new Map([[cited, name]])),
    });
    try {
      const words = () => entry(planner?.id ?? "")?.querySelector(".dispatch-markdown");
      await waitFor(() => expect(words()?.textContent).toBe(`Read ${name} first.`));
      expect(words()?.querySelector("strong")?.textContent).toBe("first");
      expect(entry(planner?.id ?? "")?.textContent).not.toContain("dispatch://");
    } finally {
      references.mockRestore();
      drawn.done();
      window.restore();
    }
  });

  test("each reply stands under the entry it answers, at one depth more and to its right", async () => {
    const window = stubWindow(1440);
    answerThreads();
    const root = FIRST_MESSAGE?.message;
    const drawn = drawThreads(threadsAt(root?.id));
    try {
      await waitFor(() => expect(entry(root?.id ?? "")).toBeDefined());
      const drawnEntries = [...document.querySelectorAll<HTMLElement>("[data-thread-entry]")];
      const depthOf = new Map(
        drawnEntries.map((element) => [
          element.getAttribute("data-thread-entry") ?? "",
          Number(element.getAttribute("data-depth")),
        ])
      );
      expect(drawnEntries[0]?.getAttribute("data-thread-entry")).toBe(root?.id ?? "");
      for (const reply of FIRST_MESSAGE?.replies ?? []) {
        const parent = reply.in_reply_to ?? "";
        expect(depthOf.get(reply.id), `${reply.id} stands at another depth`).toBe(
          (depthOf.get(parent) ?? -2) + 1
        );
        const order = drawnEntries.map((element) => element.getAttribute("data-thread-entry"));
        expect(order.indexOf(reply.id), `${reply.id} stands above its entry`).toBeGreaterThan(
          order.indexOf(parent)
        );
      }
    } finally {
      drawn.done();
      window.restore();
    }
  });
});

/** Asserts that `field` is drawn as the new page's field (`MentionComposer`'s `TONES.page.field`):
 *  the page's paper inside its hairline, in its ink, and not `main`'s recessed input, which the
 *  field draws where its host hands no palette (`MentionField`). */
function expectPageField(field: HTMLElement, which: string): void {
  expect(field, `${which} draws no field of the new page`).toHaveClass(
    pageHairBorder,
    pagePaperBg,
    pageInkText
  );
  expect(field, `${which} draws main's field`).not.toHaveClass(inputClasses(true));
}

describe("the new page's palette (R9, root R42)", () => {
  test("a settled ask opened in its record draws its record in the new page's palette", async () => {
    const window = stubWindow(1440);
    answerThreads();
    const drawn = drawThreads(threadsAt(SETTLED?.id));
    try {
      const record = await waitFor(() => {
        const found = entry(SETTLED?.id ?? "")?.querySelector<HTMLElement>(
          `[data-testid="ask-${SETTLED?.id}"]`
        );
        expect(found, "the settled ask draws no record").toBeTruthy();
        return found as HTMLElement;
      });
      // The answered record is the page's pane grey inside its hairline, where `main`'s palette
      // draws the success callout (`AskCompletionCard`'s `TONES`).
      expect(record, "the settled ask draws no record of the new page").toHaveClass(
        pageHairBorder,
        pagePaneBg
      );
      expect(record, "the settled ask draws main's success callout").not.toHaveClass(
        calloutSuccessBg
      );
    } finally {
      drawn.done();
      window.restore();
    }
  });

  test("an open question opened in its record draws Decline to answer in the new page's palette", async () => {
    const window = stubWindow(1440);
    answerThreads();
    const drawn = drawThreads(threadsAt(OPEN_ASK?.id));
    try {
      const decline = await waitFor(() =>
        within(entry(OPEN_ASK?.id ?? "") as HTMLElement).getByRole("button", {
          name: ASK_DECLINE_WORDS.decline,
        })
      );
      // Decline to answer is the page's line rule and its muted words, where `main`'s palette
      // draws the surface's border (`DeclineControl`'s `TONES`).
      expect(decline, "Decline to answer is not the new page's").toHaveClass(
        pageLineBorder,
        pageMutedText
      );
      expect(decline, "Decline to answer draws main's border").not.toHaveClass(borderDefault);
    } finally {
      drawn.done();
      window.restore();
    }
  });

  test("the box at the foot of a comment thread and the reader's comment opened to edit draw their composer in the new page's palette", async () => {
    const window = stubWindow(1440);
    answerThreads();
    const user = userEvent.setup();
    const drawn = drawThreads(threadsAt(REMARK.comment.id));
    try {
      await waitFor(() => expect(box("foot")).toBeDefined());
      expectPageField(replyField("foot"), "the box at the foot");
      await user.click(
        within(entry(REMARK.comment.id) as HTMLElement).getByRole("button", {
          name: CONTROL_LABELS.edit,
        })
      );
      expectPageField(
        within(entry(REMARK.comment.id) as HTMLElement).getByRole("textbox", {
          name: "Edit comment",
        }),
        "the comment opened to edit"
      );
    } finally {
      drawn.done();
      window.restore();
    }
  });
});

/** The reader's post, whose second block the planner's reply quotes: a thread of the comment
 *  route's second page. */
const POST = ((commentsInBeforeBody as unknown as CommentThreadPage).threads ?? []).find((row) =>
  row.comment.body.startsWith("The list reads three routes")
);
/** The reader's message, whose second block the tester's reply quotes. */
const QUOTED_MESSAGE = MESSAGES.find((row) =>
  row.message.body.startsWith("The row draws its first line")
);

/** The opened record's link to its node, where it draws one. */
const nodeLinkOf = () =>
  opened()?.querySelector<HTMLAnchorElement>("[data-thread-node-link]") ?? undefined;

/** Waits for the opened record to draw its link to its node, and answers it. */
function drawnNodeLink(record: string): Promise<HTMLAnchorElement> {
  return waitFor(() => {
    const link = nodeLinkOf();
    expect(link, `the record ${record} draws no link to its node`).toBeDefined();
    return link as HTMLAnchorElement;
  });
}

describe("the link of a record on text to its node (R8)", () => {
  test("a comment record that a reply with a quote begins links to the post, and a press follows it to the post's address", async () => {
    const window = stubWindow(1440);
    const quoting = POST?.replies?.find((reply) => (reply.reply_block?.quote ?? "") !== "");
    if (POST === undefined || quoting === undefined) {
      throw new Error("The fixtures hold no post with a quoting reply.");
    }
    // The post's thread stands on All's first page, so the record opens with no lookup.
    answerThreads({ comments: () => ({ ...commentsInBody, threads: [...COMMENTS, POST] }) });
    const user = userEvent.setup();
    const drawn = drawThreads(threadsAt(quoting.id));
    try {
      const link = await drawnNodeLink(quoting.id);
      const post = `/issues/${KEY}/comments/${POST.comment.id}`;
      expect(link.textContent).toBe(THREADS_WORDS.open_in_conversation);
      expect(link.getAttribute("href")).toBe(post);
      expect(link, "the link is not drawn in the page's muted words").toHaveClass(pageMutedText);
      await user.click(link);
      expect(drawn.address(), "a press on the link moved the address elsewhere").toBe(post);
    } finally {
      drawn.done();
      window.restore();
    }
  });

  test("a message record that a reply with a quote begins links to the message it answers", async () => {
    const window = stubWindow(1440);
    const quoting = QUOTED_MESSAGE?.replies?.[0];
    if (QUOTED_MESSAGE === undefined || quoting === undefined) {
      throw new Error("The fixtures hold no message with a quoting reply.");
    }
    answerThreads();
    const drawn = drawThreads(threadsAt(quoting.id));
    try {
      const link = await drawnNodeLink(quoting.id);
      expect(link.textContent).toBe(THREADS_WORDS.open_in_conversation);
      expect(link.getAttribute("href")).toBe(
        `/issues/${KEY}/messages/${QUOTED_MESSAGE.message.id}`
      );
    } finally {
      drawn.done();
      window.restore();
    }
  });

  test("a record on no text, and a comment on text whose link is owed, draw no link", async () => {
    const window = stubWindow(1440);
    answerThreads();
    try {
      // The comment on the spec's text links to no node: its link is owed until
      // dispatch://INFRA-1253's node anchor is on the base.
      for (const id of [REMARK.comment.id, TEXT.comment.id]) {
        const drawn = drawThreads(threadsAt(id));
        try {
          await waitFor(() => expect(entry(id), `the record ${id} is not drawn`).toBeDefined());
          expect(nodeLinkOf(), `the record ${id} draws a link to a node`).toBeUndefined();
        } finally {
          drawn.done();
        }
      }
    } finally {
      window.restore();
    }
  });
});
