/**
 * The range a shift-click takes in the Inbox's selection (`useInboxSelection`'s `markRange`): its
 * anchor, the state it takes, what a second range from the same anchor gives back, and the row
 * marked last, which no range moves.
 */

import { expect, test } from "bun:test";
import { QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook } from "@testing-library/react";
import { createElement, type ReactNode } from "react";

import { whoAmIQuery } from "../../api/queries";
import { DispatchQueryClient } from "../../api/query-client";
import { replyRow } from "../threads/replies-fixture";
import { type InboxListRow, rowId } from "./list-row";
import { useInboxSelection } from "./useInboxSelection";

/** What the list holds: `listed`, every row it lists, and `drawn`, the ids of the rows it
 *  draws, in its order. A test changes either and renders again. */
interface List {
  drawn: string[];
  listed: InboxListRow[];
}

/** A list that lists and draws the threads `ids`, in that order. */
function listOf(...ids: string[]): List {
  const listed: InboxListRow[] = ids.map((id) => ({ kind: "thread", thread: replyRow(id) }));
  return { drawn: listed.map((row) => rowId(row)), listed };
}

/** The selection of `list`, under Dispatch's own client signed in as alice. */
function renderSelection(list: List) {
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  const hook = renderHook(
    () =>
      useInboxSelection({
        asOf: undefined,
        candidates: list.listed,
        drawnIds: new Set(list.drawn),
        filters: {},
        order: () => list.drawn,
        rows: new Map(list.listed.map((row) => [rowId(row), row])),
        showDismissed: false,
      }),
    {
      wrapper: ({ children }: { children: ReactNode }) =>
        createElement(QueryClientProvider, { client }, children),
    }
  );
  /** The thread ids marked, in the order the list holds them. */
  const marked = () =>
    list.listed
      .filter((row) => hook.result.current.marked.has(rowId(row)))
      .map((row) => (row.kind === "thread" ? row.thread.thread_id : row.ask.id));
  return { hook, marked };
}

/** A thread's row id, as the selection keys it. */
const row = (threadId: string) => rowId({ kind: "thread", thread: replyRow(threadId) });

test("a mark without the shift key sets the anchor, by a mark or by an unmark", () => {
  const list = listOf("c1", "c2", "c3", "c4", "c5", "c6");
  const { hook, marked } = renderSelection(list);
  act(() => hook.result.current.onMark(row("c2")));
  act(() => hook.result.current.markRange(row("c4"), true));
  expect(marked()).toEqual(["c2", "c3", "c4"]);

  // c5 marked and unmarked again: the unmark is the anchor, and the range takes the state the
  // clicked row moves to, marked.
  act(() => hook.result.current.onMark(row("c5")));
  act(() => hook.result.current.onMark(row("c5")));
  act(() => hook.result.current.markRange(row("c6"), true));
  expect(marked()).toEqual(["c2", "c3", "c4", "c5", "c6"]);
});

test("a second range from one anchor gives back the rows of the first it does not hold, and a row marked at the anchor unmarks the range", () => {
  const list = listOf("c1", "c2", "c3", "c4", "c5", "c6", "c7", "c8");
  const { hook, marked } = renderSelection(list);
  act(() => hook.result.current.onMark(row("c8")));
  act(() => hook.result.current.onMark(row("c2")));
  act(() => hook.result.current.markRange(row("c6"), true));
  expect(marked()).toEqual(["c2", "c3", "c4", "c5", "c6", "c8"]);
  // c4 is marked now but was not at the anchor, so the range is marked again, and c5 and c6
  // go back to how they stood then: unmarked. c8, outside both ranges, keeps its mark.
  act(() => hook.result.current.markRange(row("c4"), true));
  expect(marked()).toEqual(["c2", "c3", "c4", "c8"]);
  // c8 was marked at the anchor, so the range to it is unmarked, the anchor's own row with it.
  act(() => hook.result.current.markRange(row("c8"), true));
  expect(marked()).toEqual([]);
});

test("a row that arrived after the anchor and leaves the range is unmarked", () => {
  const list = listOf("c1", "c2", "c3", "c4", "c5");
  const { hook, marked } = renderSelection(list);
  act(() => hook.result.current.onMark(row("c2")));
  // n1 arrives between c3 and c4 after the anchor was set, so it was no row at the anchor.
  const arrived: InboxListRow = { kind: "thread", thread: replyRow("n1") };
  list.listed = [...list.listed.slice(0, 3), arrived, ...list.listed.slice(3)];
  list.drawn = list.listed.map((each) => rowId(each));
  hook.rerender();
  act(() => hook.result.current.markRange(row("c5"), true));
  expect(marked()).toEqual(["c2", "c3", "n1", "c4", "c5"]);
  act(() => hook.result.current.markRange(row("c3"), true));
  expect(marked()).toEqual(["c2", "c3"]);
});

test("Select all and Clear selection leave no anchor, so the next range is its own row, which becomes the anchor", () => {
  const list = listOf("c1", "c2", "c3", "c4", "c5", "c6");
  const { hook, marked } = renderSelection(list);
  act(() => hook.result.current.onMark(row("c2")));
  act(() => hook.result.current.clearSelection());
  act(() => hook.result.current.markRange(row("c4"), true));
  expect(marked()).toEqual(["c4"]);

  act(() => hook.result.current.selectAll());
  expect(hook.result.current.allRows).toBe(true);
  act(() => hook.result.current.markRange(row("c3"), true));
  expect(hook.result.current.allRows).toBe(false);
  expect(marked()).toEqual(["c1", "c2", "c4", "c5", "c6"]);
  // c3 is the anchor now: c5 was marked there, so the range from c3 to c5 is unmarked.
  act(() => hook.result.current.markRange(row("c5"), true));
  expect(marked()).toEqual(["c1", "c2", "c6"]);
});

test("a range that starts no anchor, as Shift+J and Shift+K take one, changes nothing where no anchor is drawn", () => {
  const list = listOf("c1", "c2", "c3", "c4", "c5", "c6");
  const { hook, marked } = renderSelection(list);
  act(() => hook.result.current.onMark(row("c2")));
  act(() => hook.result.current.clearSelection());
  // After Clear selection there is no anchor: the range ticks no row and sets no anchor.
  act(() => hook.result.current.markRange(row("c3"), false));
  expect(marked()).toEqual([]);
  // So the next shift-click ticks its own row alone, not the run from c3 to it.
  act(() => hook.result.current.markRange(row("c5"), true));
  expect(marked()).toEqual(["c5"]);
  // With an anchor drawn, the same range runs from it as any other does.
  act(() => hook.result.current.markRange(row("c3"), false));
  expect(marked()).toEqual(["c3", "c4", "c5"]);
  // An anchor the list no longer draws is none either.
  list.drawn = list.drawn.filter((id) => id !== row("c5"));
  hook.rerender();
  act(() => hook.result.current.markRange(row("c1"), false));
  expect(marked()).toEqual(["c3", "c4", "c5"]);
});

test("an anchor the list no longer draws makes a range of the clicked row alone, which becomes the anchor", () => {
  const list = listOf("c1", "c2", "c3", "c4", "c5");
  const { hook, marked } = renderSelection(list);
  act(() => hook.result.current.onMark(row("c2")));
  // A filter hides c2: the list still holds it, and no longer draws it.
  list.drawn = list.drawn.filter((id) => id !== row("c2"));
  hook.rerender();
  act(() => hook.result.current.markRange(row("c4"), true));
  expect(marked()).toEqual(["c2", "c4"]);
  act(() => hook.result.current.markRange(row("c5"), true));
  expect(marked()).toEqual(["c2", "c4", "c5"]);
});

test("a range marks rows without moving lastMarked", () => {
  const list = listOf("c1", "c2", "c3", "c4", "c5");
  const { hook, marked } = renderSelection(list);
  act(() => hook.result.current.onMark(row("c2")));
  expect(hook.result.current.lastMarked()).toBe(row("c2"));
  act(() => hook.result.current.markRange(row("c5"), true));
  expect(marked()).toEqual(["c2", "c3", "c4", "c5"]);
  expect(hook.result.current.lastMarked()).toBe(row("c2"));
  // A range with no anchor marks its own row alone, and that moves nothing either.
  act(() => hook.result.current.clearSelection());
  act(() => hook.result.current.markRange(row("c4"), true));
  expect(marked()).toEqual(["c4"]);
  expect(hook.result.current.lastMarked()).toBe(row("c2"));
});
