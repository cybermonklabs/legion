import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { act, cleanup, type RenderResult, render, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { api } from "../../api/client";
import { DESKTOP_WIDTH, stubViewport } from "../margin/margin-fixture";
import { parseIssuePath } from "../refs/routes";
import { decisionsRead, issueDetails, KEY } from "./decisions-filter/decisions-fixtures";
import { presetPath } from "./issue-address";
import { IssueTreeOwnerProvider } from "./issue-tree-owner";
import { TWO_THREADS, waitInAct } from "./outline-fixtures";
import { STATE_LINES } from "./page-words";
import { ProvidersSlot } from "./slots/providers";
import { StageSlot } from "./slots/stage";
import {
  AppProviders,
  commentNodes,
  focusedRow,
  REFUSED,
  retryTakesFocus,
  stubReads,
  treeOf,
} from "./slots/stage-fixtures";

/**
 * The first drawing of the outline's rows and its scroll ("The first drawing and its scroll" of
 * the plan), rows 2 and 6. The outline draws its first rows once every read that can place a row
 * or say which row the page's first focus goes to has answered - the tree read, which answers the
 * comments too, the asks, the decisions and the reader's login - so the first commit of rows
 * draws them where they stay; the coverage, the results and the messages only mark rows placed
 * already, and are not waited on. A read refused is an answer too. The page's first focus lands
 * once every read the rows come from has answered, on a row drawn.
 */

/** The reads that can place a row of the outline or say where its first focus goes, beside the
 *  tree read, each by the api client's read of it and what it answers here. */
const PLACING = {
  asks: { read: "listIssueAskThreads", answer: () => [] },
  decisions: { read: "getIssueDecisions", answer: () => decisionsRead([]) },
  whoami: {
    read: "whoAmI",
    answer: () => ({ event_head: "0", kind: "user", login: "alice" }),
  },
} as const satisfies Record<
  string,
  { readonly answer: () => unknown; readonly read: keyof typeof api }
>;

let stubs: { mockRestore(): void }[] = [];
let restoreViewport: () => void = () => {};

/** Every read the stage reaches, answered (`stubReads`): the comments with the issue's two
 *  threads, the tree read with them, and each view's read (`?view=`) with an empty view. Each case
 *  of a test answers them again, so no read held or refused in one is held or refused in the
 *  next. */
function answerEveryRead(): void {
  stubs.push(
    ...stubReads(),
    spyOn(api, "listComments").mockResolvedValue([...TWO_THREADS]),
    spyOn(api, "getIssueTree").mockImplementation(async (_key, query = {}) =>
      query.view === undefined ? treeOf(commentNodes(TWO_THREADS)) : treeOf([])
    )
  );
}

// At 1440 px.
beforeEach(() => {
  stubs = [];
  restoreViewport = stubViewport(DESKTOP_WIDTH);
});
afterEach(() => {
  cleanup();
  restoreViewport();
  for (const stub of stubs.reverse()) stub.mockRestore();
});

/** Holds the api client's read `read` until `answer` is called with its answer, or `refuse`. */
function hold(read: keyof typeof api) {
  const held = Promise.withResolvers<never>();
  stubs.push(spyOn(api, read).mockReturnValue(held.promise as never));
  return {
    answer: (value: unknown) => act(async () => held.resolve(value as never)),
    refuse: () => act(async () => held.reject(REFUSED)),
  };
}

/** The page's slots that draw the outline, as the page's root draws them (`IssueOutlinePage.tsx`)
 *  at the address the router is at: the stage, under the page's providers and the tree's owner. */
function PageStage(): ReactNode {
  const { pathname, search } = useLocation();
  const route = parseIssuePath(pathname, search);
  if (route === undefined) throw new Error(`${pathname} names no route`);
  const issue = issueDetails();
  return (
    <ProvidersSlot issue={issue} route={route}>
      <IssueTreeOwnerProvider issue={issue}>
        <StageSlot issue={issue} route={route} />
      </IssueTreeOwnerProvider>
    </ProvidersSlot>
  );
}

/** The page's stage at Conversation's address, with a fresh client that tries no read again. */
function drawConversation(): { readonly client: QueryClient; readonly view: RenderResult } {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={[presetPath(KEY, "conversation")]}>
      <AppProviders client={client}>
        <PageStage />
      </AppProviders>
    </MemoryRouter>
  );
  return { client, view };
}

/** The node ids of the outline's rows in `container`, in drawing order. */
function rowsIn(container: HTMLElement): string[] {
  return [...container.querySelectorAll<HTMLElement>("[data-issue-body] [data-node-row]")].map(
    (row) => row.dataset.nodeId ?? ""
  );
}

/** What the outline drew at its first commit of rows: the rows, and the failed reads' lines over
 *  them, read as the commit lands (a `MutationObserver`, whose callback runs before any frame). */
function watchFirstRows(container: HTMLElement) {
  const first: { lines?: string[]; rows?: string[] } = {};
  const observer = new MutationObserver(() => {
    if (first.rows !== undefined) return;
    const rows = rowsIn(container);
    if (rows.length === 0) return;
    first.rows = rows;
    first.lines = [...container.querySelectorAll("[data-issue-body] [data-failed-read]")].map(
      (line) => line.querySelector('[role="alert"]')?.textContent ?? ""
    );
  });
  observer.observe(container, { childList: true, subtree: true });
  return { first, stop: () => observer.disconnect() };
}

/** Every call that scrolls the page or an element, from now until `restore`. */
function watchScrolls() {
  const spies = [
    spyOn(window, "scrollBy"),
    spyOn(window, "scrollTo"),
    spyOn(Element.prototype, "scrollIntoView"),
  ];
  return {
    made: () => spies.reduce((all, spy) => all + spy.mock.calls.length, 0),
    restore: () => {
      for (const spy of spies) spy.mockRestore();
    },
  };
}

// Row 2. Conversation, no question waiting on the reader: each read that can place a row is held
// in turn, the last to answer. While it is out the outline draws no row; once it answers, the
// first commit of rows draws every row the load draws, in its order, and the first of them takes
// the focus. Nothing scrolls, then or once the names the rows cite answer after.
test("row 2: Conversation with no question waiting: the first commit of rows gives the first row the focus, and nothing scrolls", async () => {
  for (const [name, placing] of Object.entries(PLACING)) {
    const where = `with the ${name} read held`;
    answerEveryRead();
    const held = hold(placing.read);
    const scrolls = watchScrolls();
    const { client, view } = drawConversation();
    const watched = watchFirstRows(view.container);
    try {
      // Every read but the held one has answered.
      await waitFor(() => {
        expect(api.getIssueTree, `the tree was never read ${where}`).toHaveBeenCalled();
        expect(client.isFetching(), `a read other than the one held is out ${where}`).toBe(1);
      });
      await waitInAct(20);
      expect(rowsIn(view.container), `the outline drew rows ${where}`).toEqual([]);

      await held.answer(placing.answer());
      const first = await waitFor(() => {
        const rows = watched.first.rows;
        expect(rows, `the outline drew no row once the ${name} read answered`).toBeDefined();
        return rows ?? [];
      });
      expect(first, `the first commit of rows drew another set ${where}`).toEqual(
        rowsIn(view.container)
      );
      await waitFor(() =>
        expect(focusedRow(), `the first row took no focus ${where}`).toBe(first[0])
      );
      expect(scrolls.made(), `the first drawing scrolled ${where}`).toBe(0);

      await waitFor(() => expect(client.isFetching(), `a read is still out ${where}`).toBe(0));
      await waitInAct(50);
      expect(rowsIn(view.container), `a later read changed the rows ${where}`).toEqual(first);
      expect(focusedRow(), `a later read moved the focus ${where}`).toBe(first[0]);
      expect(scrolls.made(), `a later read scrolled ${where}`).toBe(0);
    } finally {
      watched.stop();
      view.unmount();
      scrolls.restore();
      client.clear();
    }
  }
});

/** The reads that can place a row and draw a line of their own where they stand refused, each by
 *  the api client's read of it and the words its line says (`outline-reads.ts`'s `besideReads`). */
const REFUSABLE = [
  { line: STATE_LINES.asks_unreadable, name: "asks", read: "listIssueAskThreads" },
  { line: STATE_LINES.messages_unreadable, name: "messages", read: "listIssueMessages" },
  { line: STATE_LINES.issue_decisions_unreadable, name: "decisions", read: "getIssueDecisions" },
] as const satisfies readonly {
  readonly line: string;
  readonly name: string;
  readonly read: keyof typeof api;
}[];

// Row 6. A read the rows come from is refused while the reader's login, which can place the first
// focus too, is still out: the refusal is an answer, so the rows wait for the login alone, and
// once it answers the first commit of rows draws them with the refused read's line over them. The
// opening lands as at the base: with no question that waits on the reader drawn, on that line's
// Retry.
test("row 6: a read the rows come from refused: the rows draw with its line, and the opening lands on its Retry", async () => {
  for (const refused of REFUSABLE) {
    const where = `with the ${refused.name} read refused`;
    answerEveryRead();
    const read = hold(refused.read);
    const who = hold(PLACING.whoami.read);
    const { client, view } = drawConversation();
    const watched = watchFirstRows(view.container);
    try {
      await read.refuse();
      await waitFor(() => {
        expect(api.getIssueTree, `the tree was never read ${where}`).toHaveBeenCalled();
        expect(client.isFetching(), `a read other than the login is out ${where}`).toBe(1);
      });
      await waitInAct(20);
      expect(rowsIn(view.container), `the outline drew rows with the login out ${where}`).toEqual(
        []
      );

      await who.answer(PLACING.whoami.answer());
      await waitFor(() =>
        expect(watched.first.rows, `the outline drew no row ${where}`).toEqual(
          rowsIn(view.container)
        )
      );
      expect(watched.first.rows?.length, `the outline drew no row ${where}`).toBeGreaterThan(0);
      expect(
        watched.first.lines,
        `the first commit of rows drew no line of the refused read ${where}`
      ).toEqual([refused.line]);
      await retryTakesFocus(view, refused.line, `the opening ${where}`);
    } finally {
      watched.stop();
      view.unmount();
      client.clear();
    }
  }
});
