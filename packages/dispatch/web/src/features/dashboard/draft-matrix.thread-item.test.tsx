/**
 * The draft matrix, Dashboard's waiting items. A host move leaves a draft at its own thread
 * or ask. A box drawn for a different thread does not inherit it; a box drawn again for the
 * original thread shows it. Comment and message lifetimes also keep refused and stopped sends
 * (with their notes); the ask stores keep text alone, as in the Inbox ask matrix.
 *
 * | Move                                   | comment | message | ask reply | ask answer |
 * | -------------------------------------- | ------- | ------- | --------- | ---------- |
 * | A thread read waits, then answers      | typed   | typed   | typed     | -          |
 * | The next five are shown                | typed   | typed   | typed     | -          |
 * | An item leaves the list, then returns  | typed   | typed   | typed     | typed      |
 * | The root is deleted                    | typed   | typed   | -         | -          |
 * | The ask opens again, then settles      | -       | -       | typed     | -          |
 * | The page reloaded, then back           | all     | all     | typed     | -          |
 *
 * `draft-matrix.thread-item.coverage.ts` ties the census to these cells and its numbered reasons.
 */
import { afterEach, beforeAll, expect, spyOn, test } from "bun:test";
import type { QueryClient } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { useState } from "react";
import { MemoryRouter } from "react-router-dom";

import {
  BODY,
  commentA,
  endAsAReload,
  expectedBack,
  HELD,
  type Held,
  heldIn,
  newQueryClient,
  REFUSAL,
  REFUSED_NOTE,
  signedInAsReader,
} from "../../__tests__/draft-matrix-fixture";
import { messageCorrectionFields } from "../../__tests__/message-fixture";
import { sentRequest, stopSend } from "../../__tests__/stop-late-write";
import { api } from "../../api/client";
import type { IssueAskRow, IssueDetails, Message } from "../../api/types";
import { answeredAsk, thread as askRead } from "../inbox/ask-card-fixture";
import { NoTooltips } from "../issue-page/NoTooltips";
import { AppProviders } from "../issue-page/slots/stage-fixtures";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { NOTHING_PUBLISHED } from "../refs/reference-names";
import { replyRow } from "../threads/replies-fixture";
import asksOpenBody from "./fixtures/asks-open.json";
import issueBody from "./fixtures/issue.json";
import { replySendKey, threadQuery } from "./ThreadItem";
import type { WaitingItem } from "./useWaitingItems";
import { WAITING_STEP, WaitingList } from "./WaitingList";

const HOST = "Dashboard thread item";
const KINDS = ["comment", "message", "ask"] as const;
type ThreadKind = (typeof KINDS)[number];
type Thread = Extract<WaitingItem, { kind: "thread" }>;

const ISSUE = { ...(issueBody as unknown as IssueDetails), key: "ACME-1" };
const AUTHOR = { id: "writer", kind: "user" } as const;
const AT = "2026-11-25T10:00:00Z";
const REPLY_AT = "2026-11-25T11:00:00Z";

function threadItem(kind: ThreadKind, id = `${kind}-root`): Thread {
  const row = replyRow(id, {
    kind,
    read_through: AT,
    ref: `dispatch://${ISSUE.key}/${kind}/${id}`,
    reply: { ...replyRow(id).reply, author: AUTHOR, created_at: REPLY_AT },
  });
  return { at: REPLY_AT, id, kind: "thread", row };
}

function question(id: string): WaitingItem {
  const base = asksOpenBody[0] as unknown as IssueAskRow;
  return { ask: { ...base, id, issue_key: ISSUE.key }, at: AT, id, kind: "question" };
}

function comment(id: string) {
  return { ...commentA, author: AUTHOR, id, issue_key: ISSUE.key };
}

function message(id: string): Message {
  return {
    ...messageCorrectionFields(),
    author: AUTHOR,
    body: "Thread root",
    broadcast_id: null,
    created_at: AT,
    deliveries: [],
    id,
    in_reply_to: null,
    issue_key: ISSUE.key,
    reply_block: null,
    target: null,
  };
}

function ask(id: string, open = false) {
  return answeredAsk({
    author: AUTHOR,
    id,
    issue_key: ISSUE.key,
    options: [],
    ...(open ? { answer: null, state: "open" } : {}),
  });
}

const spies: { mockRestore: () => void }[] = [];
afterEach(() => {
  for (const spy of spies.splice(0)) spy.mockRestore();
});

beforeAll(async () => {
  await warmMarkdownRenderer();
});

function serveThreads(beforeRead?: () => Promise<void> | undefined): void {
  spies.push(
    spyOn(api, "listAgents").mockResolvedValue([]),
    spyOn(api, "getComment").mockImplementation(async (id) => {
      await beforeRead?.();
      return { comment: comment(id), replies: [] };
    }),
    spyOn(api, "getMessage").mockImplementation(async (_key, id) => {
      await beforeRead?.();
      return { message: message(id), replies: [] };
    }),
    spyOn(api, "getAsk").mockImplementation(async (id) => {
      await beforeRead?.();
      return askRead(ask(id));
    })
  );
}

/** Feed the list the rows its parent would show, with the same paging state as DashboardTab. */
function PagedList({ items }: { items: readonly WaitingItem[] | undefined }) {
  const [drawn, setDrawn] = useState(WAITING_STEP);
  return (
    <WaitingList
      decisionsDocument={undefined}
      drawn={drawn}
      issue={ISSUE}
      items={items}
      names={{ excerptOf: () => "", published: NOTHING_PUBLISHED }}
      onMore={() => setDrawn((count) => count + WAITING_STEP)}
      page="today"
      viewer="alice"
    />
  );
}

function drawList(items: readonly WaitingItem[], client = signedInAsReader(newQueryClient())) {
  const user = userEvent.setup();
  let shownClient = client;
  const tree = (shown: readonly WaitingItem[] | undefined) => (
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <AppProviders client={shownClient}>
        <NoTooltips>
          <PagedList items={shown} />
        </NoTooltips>
      </AppProviders>
    </MemoryRouter>
  );
  let view = render(tree(items));
  return {
    move: (shown: readonly WaitingItem[] | undefined) => view.rerender(tree(shown)),
    reload: () => {
      endAsAReload(() => view.unmount());
      shownClient = signedInAsReader(newQueryClient());
      view = render(tree(items));
    },
    unmount: () => view.unmount(),
    user,
  };
}

const itemId = (item: WaitingItem) => `dashboard-item-${item.kind}-${item.id}`;
const itemView = (item: WaitingItem) => within(screen.getByTestId(itemId(item)));
const replyBox = (item: Thread) =>
  itemView(item).findByRole<HTMLTextAreaElement>("textbox", { name: "Reply" });
const answerBox = (item: WaitingItem) =>
  itemView(item).findByLabelText<HTMLTextAreaElement>("Your answer");

async function writeDraft(user: UserEvent, field: HTMLTextAreaElement): Promise<void> {
  await user.click(field);
  await user.paste(BODY);
  expect(field.value).toBe(BODY);
}

for (const kind of KINDS) {
  test(`${HOST} · a thread read waits, then answers · ${kind} · typed`, async () => {
    let pending: Promise<void> | undefined;
    serveThreads(() => pending);
    const item = threadItem(kind);
    const client = signedInAsReader(newQueryClient());
    const page = drawList([item], client);
    const gate = Promise.withResolvers<void>();
    try {
      await writeDraft(page.user, await replyBox(item));
      // Reset just this thread, not the waiting row that draws it. The next read is held out so
      // the early return really takes the box away before the answer brings it back.
      pending = gate.promise;
      act(() => {
        // Start the request without waiting for its answer.
        void client.resetQueries({
          exact: true,
          queryKey: threadQuery(ISSUE.key, item.row).queryKey,
        });
      });
      await waitFor(() =>
        expect(itemView(item).queryByRole("textbox", { name: "Reply" })).toBeNull()
      );
      gate.resolve();
      expect((await replyBox(item)).value).toBe(BODY);
    } finally {
      gate.resolve();
      page.unmount();
    }
  });

  test(`${HOST} · the next five are shown · ${kind} · typed`, async () => {
    serveThreads();
    const item = threadItem(kind);
    const next = threadItem(kind, `${kind}-next`);
    const between = Array.from({ length: WAITING_STEP - 1 }, (_, index) => question(`q-${index}`));
    const page = drawList([item, ...between, next]);
    try {
      const original = await replyBox(item);
      await writeDraft(page.user, original);
      expect(screen.queryByTestId(itemId(next))).toBeNull();
      await page.user.click(screen.getByRole("button", { name: /^Show the next five/ }));
      expect((await replyBox(next)).value).toBe("");
      expect(await replyBox(item)).toBe(original);
      expect(original.value).toBe(BODY);
    } finally {
      page.unmount();
    }
  });

  test(`${HOST} · an item leaves the list, then returns · ${kind} · typed`, async () => {
    serveThreads();
    const item = threadItem(kind);
    const other = threadItem(kind, `${kind}-other`);
    const page = drawList([item]);
    try {
      await writeDraft(page.user, await replyBox(item));
      page.move([]);
      expect(screen.queryByTestId(itemId(item))).toBeNull();
      page.move([other]);
      expect((await replyBox(other)).value).toBe("");
      page.move(undefined);
      expect(screen.queryByTestId(itemId(other))).toBeNull();
      page.move([item]);
      expect((await replyBox(item)).value).toBe(BODY);
    } finally {
      page.unmount();
    }
  });
}

for (const kind of ["comment", "message"] as const) {
  test(`${HOST} · the root is deleted · ${kind} · typed`, async () => {
    serveThreads();
    const item = threadItem(kind);
    const client = signedInAsReader(newQueryClient());
    const page = drawList([item], client);
    try {
      await writeDraft(page.user, await replyBox(item));
      const deleted = { body: "", deleted_at: REPLY_AT, deleted_by: AUTHOR };
      act(() => {
        if (kind === "comment") {
          client.setQueryData(["comment", item.id], {
            comment: { ...comment(item.id), ...deleted },
            replies: [],
          });
        } else {
          client.setQueryData(["issue-message", ISSUE.key, item.id], {
            message: { ...message(item.id), ...deleted },
            replies: [],
          });
        }
      });
      expect((await itemView(item).findByRole("status")).textContent).toContain(BODY);
      expect(itemView(item).queryByRole("textbox", { name: "Reply" })).toBeNull();
      expect(itemView(item).getByRole("button", { name: "Discard my draft" })).toBeTruthy();
    } finally {
      page.unmount();
    }
  });

  for (const held of HELD) {
    test(`${HOST} · the page reloaded, then back · ${kind} · ${held}`, async () => {
      serveThreads();
      const item = threadItem(kind);
      const client = signedInAsReader(newQueryClient());
      await refuseOrStop(client, item, kind, held);
      const page = drawList([item], client);
      try {
        const field = await replyBox(item);
        if (held !== "stopped") await writeDraft(page.user, field);
        if (held === "refused") {
          await page.user.click(itemView(item).getByRole("button", { name: "Send" }));
          await itemView(item).findByText(REFUSED_NOTE);
        }
        const element = screen.getByTestId(itemId(item));
        expect(heldIn(element)).toEqual(expectedBack(held));
        page.reload();
        await replyBox(item);
        expect(heldIn(screen.getByTestId(itemId(item)))).toEqual(expectedBack(held));
      } finally {
        page.unmount();
      }
    });
  }
}

async function refuseOrStop(
  client: QueryClient,
  item: Thread,
  kind: "comment" | "message",
  held: Held
): Promise<void> {
  const createComment = spyOn(api, "createComment");
  const createMessage = spyOn(api, "createMessage");
  spies.push(createComment, createMessage);
  if (held === "stopped") {
    const request = sentRequest(
      BODY,
      { actor: AUTHOR, excerpt: "", id: item.id, parentKind: kind },
      { owner: { issueKey: ISSUE.key, kind: "issue" } }
    );
    await stopSend(client, replySendKey(item.id), request, [createComment, createMessage]);
  }
  createComment.mockRejectedValue(REFUSAL);
  createMessage.mockRejectedValue(REFUSAL);
}

test(`${HOST} · the page reloaded, then back · ask · typed`, async () => {
  serveThreads();
  const item = threadItem("ask");
  const page = drawList([item]);
  try {
    await writeDraft(page.user, await replyBox(item));
    page.reload();
    expect((await replyBox(item)).value).toBe(BODY);
  } finally {
    page.unmount();
  }
});

test(`${HOST} · the ask opens again, then settles · typed`, async () => {
  serveThreads();
  const item = threadItem("ask");
  const client = signedInAsReader(newQueryClient());
  const page = drawList([item], client);
  try {
    await writeDraft(page.user, await replyBox(item));
    act(() => client.setQueryData(["ask-thread", item.id], askRead(ask(item.id, true))));
    expect((await answerBox(item)).value).toBe("");
    expect(itemView(item).queryByRole("textbox", { name: "Reply" })).toBeNull();
    act(() => client.setQueryData(["ask-thread", item.id], askRead(ask(item.id))));
    expect((await replyBox(item)).value).toBe(BODY);
  } finally {
    page.unmount();
  }
});

test(`${HOST} · an answer stays with its ask across waiting items · typed`, async () => {
  serveThreads();
  const item = threadItem("ask");
  const sameAsk = question(item.id);
  const otherAsk = question("other-ask");
  const client = signedInAsReader(newQueryClient());
  client.setQueryData(["ask-thread", item.id], askRead(ask(item.id, true)));
  const page = drawList([sameAsk], client);
  try {
    await writeDraft(page.user, await answerBox(sameAsk));
    page.move([otherAsk]);
    expect((await answerBox(otherAsk)).value).toBe("");
    page.move([item]);
    expect((await answerBox(item)).value).toBe(BODY);
    page.move([sameAsk]);
    expect((await answerBox(sameAsk)).value).toBe(BODY);
  } finally {
    page.unmount();
  }
});
