import { afterEach, expect, type Mock, spyOn, test } from "bun:test";
import type { QueryClient } from "@tanstack/react-query";
import { act, type RenderResult, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";

import { api } from "../../../api/client";
import { KEY, SPEC_DOCUMENT } from "../decisions-filter/decisions-fixtures";
import { presetPath } from "../issue-address";
import { treeOf } from "../slots/stage-fixtures";
import { blockNodeId } from "../tree-drafts";
import { readOlderComments, type TreeAnswer } from "../tree-read/tree-read";
import { GATES } from "./gates";
import {
  answerSeededReads,
  columnOf,
  type DrawnPage,
  drawFrame,
  drawPage,
  newClient,
  ON_CHECK,
  ON_DECISION,
  ON_STATEMENT,
  OTHER,
  PLAN_DOCUMENT,
  REPLY_ON_STATEMENT,
  readAgain,
  rowOf,
  SERVES,
  type SeededReads,
  STATEMENT,
  scrollingPage,
  seededNodes,
  type TextThreadSeed,
  widthAt,
} from "./margin-fixtures";
import { TEXT_COMPOSER_ATTRIBUTE, useTextComposer } from "./TextMargin";

/**
 * R2: when the column of comments on text is drawn, and how wide the main column is then. Each
 * case is a row of the plan's table, and each that asserts the column away first asserts it
 * drawn, so a page that never draws one fails the row on its own words. The widths are the
 * classes the column and the main column carry, since happy-dom lays nothing out; the browser
 * suite measures them (c-margin (1), (6), (7)).
 */

/** The margin's name for its column (`page-words-margin.ts`, R11). */
const COLUMN = "Comments on text";

/** The statement's row in the spec, which `ON_STATEMENT` is on. */
const STATEMENT_NODE = blockNodeId(SPEC_DOCUMENT.slug, STATEMENT);

let stubs: { mockRestore(): void }[] = [];
afterEach(() => {
  for (const stub of stubs) stub.mockRestore();
  stubs = [];
});

/** The page at `preset` with `reads` answered, once its first row is drawn. */
async function drawn(
  reads: SeededReads,
  preset: Parameters<typeof drawPage>[1],
  options: Parameters<typeof drawPage>[2] = {}
): Promise<{ readonly client: QueryClient; readonly view: DrawnPage }> {
  stubs = answerSeededReads(reads);
  const client = newClient();
  const view = drawPage(client, preset, options);
  await waitFor(() => expect(view.container.querySelector("[data-node-id]")).not.toBeNull());
  return { client, view };
}

/** The column, drawn, or a failure that says it is not. */
async function columnDrawn(view: RenderResult, said: string): Promise<HTMLElement> {
  const column = await columnOf(view, COLUMN);
  expect(column, said).not.toBeNull();
  return column as HTMLElement;
}

/** The issue's main column: the box the stage and the foot are drawn in. */
function mainColumn(view: RenderResult): HTMLElement {
  return view.getByTestId("issue-column");
}

/** The cards of the threads in `column`. */
function cardsOf(column: HTMLElement): HTMLElement[] {
  return [...column.querySelectorAll<HTMLElement>("[data-margin-item]")];
}

/** Whether `element` is out of the page's reach: inert, or inside what is. */
function inert(element: Element): boolean {
  return element.closest("[inert]") !== null;
}

test("no column, the first comments page that holds a thread of a drawn node loads: the column is drawn and the main column narrows by its width", async () => {
  const reads: SeededReads = { threads: [] };
  const { client, view } = await drawn(reads, "conversation");
  try {
    expect(await columnOf(view, COLUMN, 300), "a column is drawn with no thread").toBeNull();
    reads.threads = [ON_STATEMENT];
    await readAgain(client);
    const column = await columnDrawn(
      view,
      "no column is drawn once a thread of a drawn node loads"
    );
    const main = mainColumn(view);
    expect(column.parentElement, "the column is not beside the main column").toBe(
      main.parentElement
    );
    expect(
      main.compareDocumentPosition(column) & Node.DOCUMENT_POSITION_FOLLOWING,
      "the column does not follow the main column"
    ).not.toBe(0);
    expect(main.className, "the main column does not give up what the column takes").toMatch(
      /\bmin-w-0\b.*\bflex-1\b|\bflex-1\b.*\bmin-w-0\b/
    );
    expect(widthAt(column, view.window.width()), "the column's width at 1440 px").toBe(320);
  } finally {
    view.finish();
    client.clear();
  }
});

test("column, the last thread of the view's drawn nodes goes and no composer is open: no column and the main column takes its whole width", async () => {
  const reads: SeededReads = { threads: [ON_STATEMENT] };
  const { client, view } = await drawn(reads, "conversation");
  try {
    await columnDrawn(view, "no column is drawn for the seeded thread");
    reads.threads = [];
    await readAgain(client);
    await waitFor(() =>
      expect(
        view.queryByRole("region", { hidden: true, name: COLUMN }),
        "the column stays with no thread left"
      ).toBeNull()
    );
    expect(
      mainColumn(view).nextElementSibling,
      "something stands beside the main column"
    ).toBeNull();
  } finally {
    view.finish();
    client.clear();
  }
});

/** A control that opens the composer of this part on the statement's words, as the control of the
 *  selection does (`SelectionComment`). */
function ComposerOpener(): ReactNode {
  const composer = useTextComposer();
  return (
    <button
      onClick={() =>
        composer.open({ block: STATEMENT, nodeId: STATEMENT_NODE, occurrence: 0, quote: "page" })
      }
      type="button"
    >
      open a composer
    </button>
  );
}

test("no column, a composer opens in a view with no thread: the column is drawn", async () => {
  stubs = answerSeededReads({ threads: [] });
  const client = newClient();
  const view = drawFrame(client, "conversation", <ComposerOpener />);
  try {
    await waitFor(() => expect(rowOf(view, STATEMENT_NODE)).not.toBeNull());
    expect(await columnOf(view, COLUMN, 300), "a column is drawn with no thread").toBeNull();
    await userEvent.setup().click(view.getByRole("button", { name: "open a composer" }));
    const column = await columnDrawn(view, "no column is drawn for an open composer");
    expect(
      column.querySelector(`[${TEXT_COMPOSER_ATTRIBUTE}]`),
      "the column holds no composer"
    ).not.toBeNull();
  } finally {
    view.finish();
    client.clear();
  }
});

test("column, the view changes to one that draws no node with a thread: no column", async () => {
  const reads: SeededReads = { threads: [ON_DECISION] };
  const { client, view } = await drawn(reads, "decisions");
  try {
    await columnDrawn(view, "Decisions draws no column for the thread on its decision");
    await act(async () => view.held.navigate?.(presetPath(KEY, "verification")));
    await waitFor(() =>
      expect(rowOf(view, STATEMENT_NODE), "Verification drew no row").not.toBeNull()
    );
    expect(
      await columnOf(view, COLUMN, 500),
      "a column is drawn in a view that draws no node with a thread"
    ).toBeNull();
  } finally {
    view.finish();
    client.clear();
  }
});

test("column, the window narrows below 1024 px: no column", async () => {
  const { client, view } = await drawn({ threads: [ON_STATEMENT] }, "conversation");
  try {
    await columnDrawn(view, "no column is drawn at 1440 px");
    await view.window.set(1000);
    await waitFor(() =>
      expect(
        view.queryByRole("region", { hidden: true, name: COLUMN }),
        "the column stays below 1024 px"
      ).toBeNull()
    );
  } finally {
    view.finish();
    client.clear();
  }
});

// R3: each thread stands in the sheet inside its row; the row of the statement is `NodeRow`'s, part
// X's (G6).
test.skipIf(!GATES.PART_X)(
  "column, the window narrows below 1024 px: each thread is in its row's sheet",
  async () => {
    const { client, view } = await drawn({ threads: [ON_STATEMENT] }, "conversation");
    try {
      await columnDrawn(view, "no column is drawn at 1440 px");
      await view.window.set(1000);
      await waitFor(() =>
        expect(
          rowOf(view, STATEMENT_NODE)?.querySelector("[data-text-threads]"),
          "the statement's row draws no sheet"
        ).not.toBeNull()
      );
    } finally {
      view.finish();
      client.clear();
    }
  }
);

test("sheet, the window widens to 1024 px or more: a column of 280 px, and of 320 px at 1280 px", async () => {
  const { client, view } = await drawn({ threads: [ON_STATEMENT] }, "conversation", {
    width: 1000,
  });
  try {
    expect(await columnOf(view, COLUMN, 300), "a column is drawn below 1024 px").toBeNull();
    const widths: Record<number, number | undefined> = {};
    for (const width of [1024, 1279, 1280, 1440]) {
      await view.window.set(width);
      const column = await columnDrawn(view, `no column is drawn at ${width} px`);
      widths[width] = widthAt(column, width);
    }
    expect(widths, "the column's width at each window").toEqual({
      1024: 280,
      1279: 280,
      1280: 320,
      1440: 320,
    });
  } finally {
    view.finish();
    client.clear();
  }
});

test("column, Details opens: the column keeps its width, draws nothing and no Tab reaches a thread, and the main column does not move", async () => {
  const { client, view } = await drawn({ threads: [ON_STATEMENT] }, "conversation", {
    drawer: true,
  });
  try {
    const column = await columnDrawn(view, "no column is drawn");
    const main = mainColumn(view);
    const before = { column: column.className, main: main.className, width: widthAt(column, 1440) };
    expect(cardsOf(column).length, "the column draws no thread").toBeGreaterThan(0);
    await act(async () => view.held.drawer?.setOpen(true));
    const kept = view.queryByRole("region", { hidden: true, name: COLUMN });
    expect(kept, "the column left the page with Details").not.toBeNull();
    expect(
      { column: kept?.className, main: main.className, width: kept && widthAt(kept, 1440) },
      "Details moved the column or the main column"
    ).toEqual(before);
    for (const card of cardsOf(column)) {
      expect(
        inert(card) || card.closest<HTMLElement>("[style]")?.style.visibility === "hidden",
        "a card is drawn"
      ).toBe(true);
    }
    const reachable = [
      ...column.querySelectorAll("a[href], button, textarea, input, [tabindex]"),
    ].filter((element) => !inert(element));
    expect(reachable.length, "Tab reaches a thread under Details").toBe(0);
  } finally {
    view.finish();
    client.clear();
  }
});

test("column hidden by Details, Details closes: the threads are drawn again at their tops", async () => {
  const { client, view } = await drawn({ threads: [ON_STATEMENT] }, "conversation", {
    drawer: true,
  });
  try {
    const column = await columnDrawn(view, "no column is drawn");
    const tops = cardsOf(column).map((card) => card.style.top);
    expect(tops.length, "the column draws no thread").toBeGreaterThan(0);
    await act(async () => view.held.drawer?.setOpen(true));
    await act(async () => view.held.drawer?.setOpen(false));
    const again = cardsOf(view.getByRole("region", { name: COLUMN }));
    expect(
      again.map((card) => card.style.top),
      "the threads moved from their tops"
    ).toEqual(tops);
    expect(again.filter(inert), "a thread is still out of reach").toEqual([]);
  } finally {
    view.finish();
    client.clear();
  }
});

test("column hidden by Details, an address of R8 names a thread on text: Details closes, then the thread stands at its words with the focus on the named item", async () => {
  const { client, view } = await drawn({ threads: [ON_STATEMENT] }, "conversation", {
    drawer: true,
  });
  try {
    await columnDrawn(view, "no column is drawn");
    await act(async () => view.held.drawer?.setOpen(true));
    await act(async () =>
      view.held.navigate?.(`${presetPath(KEY, "conversation")}?comment=${ON_STATEMENT.comment.id}`)
    );
    await waitFor(() => expect(view.held.drawer?.open, "Details stays open").toBe(false));
    const column = view.getByRole("region", { name: COLUMN });
    await waitFor(() =>
      expect(
        document.activeElement?.closest("[data-margin-item]"),
        "the focus is not on the thread's item"
      ).not.toBeNull()
    );
    expect(column.contains(document.activeElement), "the focus is outside the column").toBe(true);
  } finally {
    view.finish();
    client.clear();
  }
});

// R8: an address of a thread on text lands the reader on it from elsewhere. The item takes the focus
// with no scroll of the browser's own, which goes to the head of the column where the card stood
// before the stack placed it, and is brought to the middle of the window once it is placed.
test("an address of R8 names a thread on text: the item takes the focus, the page is where it was until the item is placed, and it is brought to the middle of the window", async () => {
  const page = scrollingPage(0, 0);
  const { client, view } = await drawn({ threads: [ON_STATEMENT] }, "conversation");
  try {
    await columnDrawn(view, "no column is drawn");
    await act(async () =>
      view.held.navigate?.(`${presetPath(KEY, "conversation")}?comment=${ON_STATEMENT.comment.id}`)
    );
    const item = await waitFor(() => {
      const focused = document.activeElement?.closest<HTMLElement>("[data-margin-item]") ?? null;
      expect(focused, "the focus is not on the thread's item").not.toBeNull();
      return focused as HTMLElement;
    });
    await waitFor(() =>
      expect(
        page.revealed.some(
          ({ element, options }) =>
            item.contains(element) && (options as { block?: string }).block === "center"
        ),
        "the item was not brought to the middle of the window"
      ).toBe(true)
    );
  } finally {
    page.restore();
    view.finish();
    client.clear();
  }
});

// R2: a thread stands level with the first line of its quoted words in the node's row, or level with
// the row's first line where the row does not draw those words. A view that cuts a node's words
// (`CutWords`) clips what stands past the cut, which is in the text and in no line the reader sees.
// happy-dom lays nothing out, so the case gives the row at 100 px, its words' box 144 px tall under
// it, and the quoted words' first line at `firstLine`.
async function threadTopWithWordsAt(firstLine: number): Promise<number> {
  const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    if (this.hasAttribute("data-node-words")) return new DOMRect(0, 100, 400, 144);
    if (this.hasAttribute("data-node-id")) return new DOMRect(0, 100, 400, 200);
    return new DOMRect(0, 0, 400, 0);
  });
  const line = new DOMRect(0, firstLine, 40, 20);
  const list = Object.assign([line], { item: (index: number) => [line][index] ?? null });
  const lines = spyOn(Range.prototype, "getClientRects").mockReturnValue(
    list as unknown as DOMRectList
  );
  const { client, view } = await drawn({ threads: [ON_STATEMENT] }, "spec").finally(() =>
    stubs.push(rect, lines)
  );
  try {
    const column = await columnDrawn(view, "no column is drawn");
    const box = await waitFor(() => {
      const held = column.querySelector<HTMLElement>(
        `[data-text-thread="${CSS.escape(ON_STATEMENT.comment.id)}"]`
      );
      expect(held?.style.top, "the thread is not placed").not.toBe("");
      return held as HTMLElement;
    });
    return Number.parseFloat(box.style.top);
  } finally {
    view.finish();
    client.clear();
  }
}

test("column, the quoted words stand in the row's words past the cut: the thread stands level with the row's first line", async () => {
  expect(
    await threadTopWithWordsAt(300),
    "the thread stands level with words that no line of the row draws"
  ).toBe(100);
});

test("column, the quoted words stand in the row's words within the cut: the thread stands level with them", async () => {
  expect(await threadTopWithWordsAt(120), "the thread is not level with its words").toBe(120);
});

// R2: the column stacks its threads through `useCardStack` with the focused row's first thread as
// the stack's focus (`CardStackFocus`): that thread stands level with its words, the threads above
// it move up and those under it move down. Conversation draws the statement's row, then the
// check's, then the decision's; happy-dom lays nothing out, so they stand where the case says -
// at 100 px, 120 and 140 - and each thread's box is 150 px tall, so each box would cover the next
// one at its words.
test("column, j or k moves the focus to a row with threads: that row's first thread stands level with its words, the threads above move up and those below move down", async () => {
  const user = userEvent.setup();
  const threads = [ON_STATEMENT, ON_CHECK, ON_DECISION];
  const rowTops = new Map([
    [ON_STATEMENT.node, 100],
    [ON_CHECK.node, 120],
    [ON_DECISION.node, 140],
  ]);
  const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    return new DOMRect(0, rowTops.get(this.getAttribute("data-node-id") ?? "") ?? 0, 400, 24);
  });
  const { client, view } = await drawn({ threads }, "conversation").finally(() => stubs.push(rect));
  try {
    const column = await columnDrawn(view, "no column is drawn");
    await waitFor(() =>
      expect(cardsOf(column), "the column draws no three threads").toHaveLength(3)
    );
    const box = (seed: TextThreadSeed): HTMLElement | null =>
      column.querySelector<HTMLElement>(`[data-text-thread="${CSS.escape(seed.comment.id)}"]`);
    for (const seed of threads) {
      const held = box(seed);
      if (held !== null) Object.defineProperty(held, "offsetHeight", { value: 150 });
    }
    const tops = () => threads.map((seed) => Number.parseFloat(box(seed)?.style.top ?? ""));
    // The focus goes to the row of the plan's block, which has no thread, and then to the
    // statement's: whichever row the page opened on, the column reads its boxes' heights again.
    await act(async () => rowOf(view, blockNodeId(PLAN_DOCUMENT.slug, OTHER))?.focus());
    await act(async () => rowOf(view, ON_STATEMENT.node)?.focus());
    await waitFor(() =>
      expect(tops(), "with the statement's row in focus, its thread is not at its words").toEqual([
        100, 258, 416,
      ])
    );
    await user.keyboard("j");
    expect(
      document.activeElement?.closest("[data-node-id]")?.getAttribute("data-node-id"),
      "j did not move the focus to the check's row"
    ).toBe(ON_CHECK.node);
    await waitFor(() =>
      expect(
        tops(),
        "the check's thread is not at its words, with the statement's 8 px above it and the decision's 8 px under it"
      ).toEqual([-38, 120, 278])
    );
  } finally {
    view.finish();
    client.clear();
  }
});

// R2 and R4: a press on a control of a thread the stack pushed under the thread above it acts. The
// press takes the focus before it ends and the stack stands the box in focus level with its words,
// so, were the focus taken at once, the box would move from under the pointer and the release would
// land outside its control. The focus a pointer moves is taken up when the press ends.
test("column, a press on the Resolve of a thread the stack pushed down: the thread stays under the pointer until the press ends, then stands level with its words, and the press resolves it", async () => {
  const user = userEvent.setup();
  const threads = [ON_STATEMENT, ON_CHECK, ON_DECISION];
  const rowTops = new Map([
    [ON_STATEMENT.node, 100],
    [ON_CHECK.node, 120],
    [ON_DECISION.node, 140],
  ]);
  const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    return new DOMRect(0, rowTops.get(this.getAttribute("data-node-id") ?? "") ?? 0, 400, 24);
  });
  const resolve = spyOn(api, "resolveComment").mockResolvedValue(
    undefined as unknown as Awaited<ReturnType<typeof api.resolveComment>>
  );
  const { client, view } = await drawn({ threads }, "conversation").finally(() =>
    stubs.push(rect, resolve)
  );
  try {
    const column = await columnDrawn(view, "no column is drawn");
    await waitFor(() =>
      expect(cardsOf(column), "the column draws no three threads").toHaveLength(3)
    );
    const box = (seed: TextThreadSeed): HTMLElement | null =>
      column.querySelector<HTMLElement>(`[data-text-thread="${CSS.escape(seed.comment.id)}"]`);
    for (const seed of threads) {
      const held = box(seed);
      if (held !== null) Object.defineProperty(held, "offsetHeight", { value: 150 });
    }
    const tops = () => threads.map((seed) => Number.parseFloat(box(seed)?.style.top ?? ""));
    await act(async () => rowOf(view, blockNodeId(PLAN_DOCUMENT.slug, OTHER))?.focus());
    await act(async () => rowOf(view, ON_STATEMENT.node)?.focus());
    await waitFor(() =>
      expect(tops(), "the check's thread is not pushed under the statement's").toEqual([
        100, 258, 416,
      ])
    );
    const checkBox = box(ON_CHECK) as HTMLElement;
    const resolveButton = within(checkBox).getByRole("button", { name: "Resolve" });
    await user.pointer({ keys: "[MouseLeft>]", target: resolveButton });
    expect(document.activeElement, "the press did not give the focus to the control it is on").toBe(
      resolveButton
    );
    await act(async () => new Promise((done) => setTimeout(done, 30)));
    expect(tops(), "the thread moved from under the pointer while the press was down").toEqual([
      100, 258, 416,
    ]);
    await user.pointer({ keys: "[/MouseLeft]", target: resolveButton });
    await waitFor(() =>
      expect(tops(), "the thread is not level with its words once the press ended").toEqual([
        -38, 120, 278,
      ])
    );
    expect(resolve, "the press did not resolve the thread").toHaveBeenCalledTimes(1);
    expect(resolve.mock.calls[0]?.[0], "the press resolved another thread").toBe(
      ON_CHECK.comment.id
    );
  } finally {
    view.finish();
    client.clear();
  }
});

test("column, an older comments page loads: the threads of its roots are drawn in order", async () => {
  stubs = answerSeededReads({ threads: [] });
  const first: TreeAnswer = {
    ...treeOf(seededNodes([ON_CHECK]), SERVES),
    comments_before: "2026-11-05T01:03:00.000000Z,on-c1",
  };
  const older: TreeAnswer = {
    ...treeOf(seededNodes([ON_STATEMENT]), SERVES),
    comments_before: null,
  };
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async (_key, query) =>
    query?.comments_before === undefined ? first : older
  );
  (api.listComments as Mock<typeof api.listComments>).mockResolvedValue([
    ON_CHECK.comment,
    ON_STATEMENT.comment,
    REPLY_ON_STATEMENT,
  ]);
  const client = newClient();
  const view = drawPage(client, "conversation");
  try {
    const column = await columnDrawn(view, "no column is drawn for the first page's thread");
    expect(column.textContent, "the older page's thread is drawn before it loads").not.toContain(
      ON_STATEMENT.comment.body
    );
    await act(async () => {
      await readOlderComments(client, KEY, "conversation");
    });
    await waitFor(() => expect(column.textContent).toContain(ON_STATEMENT.comment.body));
    const text = column.textContent ?? "";
    expect(
      text.indexOf(ON_STATEMENT.comment.body),
      "the statement's thread stands after the check's, though its words come first"
    ).toBeLessThan(text.indexOf(ON_CHECK.comment.body));
  } finally {
    view.finish();
    client.clear();
  }
});

test("column, the reader resizes Details: nothing in the column or the main column moves", async () => {
  const { client, view } = await drawn({ threads: [ON_STATEMENT] }, "conversation", {
    drawer: true,
  });
  try {
    const column = await columnDrawn(view, "no column is drawn");
    await act(async () => view.held.drawer?.setOpen(true));
    const held = () => ({
      column: column.getAttribute("class"),
      main: mainColumn(view).getAttribute("class"),
      style: column.getAttribute("style"),
      tops: cardsOf(column).map((card) => card.style.top),
    });
    const before = held();
    // Details stands over the page and is as wide as the reader drags it; the page is told the
    // window's size has not changed, and nothing but the drawer's own width moves.
    const drawer = document.createElement("div");
    drawer.setAttribute("data-details", "");
    drawer.style.width = "420px";
    document.body.append(drawer);
    drawer.style.width = "560px";
    window.dispatchEvent(new Event("resize"));
    await act(async () => {});
    drawer.remove();
    expect(held(), "resizing Details moved the column or the main column").toEqual(before);
  } finally {
    view.finish();
    client.clear();
  }
});

/** The header's band, the header and the view row: what stands across the page above the row
 *  that holds the main column and the column of comments on text. */
function aboveTheRow(view: RenderResult): Record<string, HTMLElement | null> {
  return {
    band: view.container.querySelector<HTMLElement>("[data-identity-block]"),
    header: view.container.querySelector<HTMLElement>("[data-issue-header]"),
    "view row": view.queryByTestId("issue-presets"),
  };
}

test("the header and the view row keep their width when the column is drawn", async () => {
  const reads: SeededReads = { threads: [] };
  const { client, view } = await drawn(reads, "conversation");
  try {
    const classes = () =>
      Object.fromEntries(
        Object.entries(aboveTheRow(view)).map(([name, element]) => [
          name,
          `${element?.className ?? ""} | ${element?.parentElement?.className ?? ""}`,
        ])
      );
    const before = classes();
    reads.threads = [ON_STATEMENT];
    await readAgain(client);
    const column = await columnDrawn(view, "no column is drawn once a thread loads");
    for (const [name, element] of Object.entries(aboveTheRow(view))) {
      expect(element, `the page draws no ${name}`).not.toBeNull();
      expect(
        column.parentElement?.contains(element),
        `the ${name} stands in the row the column narrows`
      ).toBe(false);
    }
    expect(classes(), "the column changed the width of what stands above it").toEqual(before);
  } finally {
    view.finish();
    client.clear();
  }
});

/** The controls inside `within` that Tab stops on, in the document's order. */
function tabStopsIn(within: Element): HTMLElement[] {
  return [
    ...within.querySelectorAll<HTMLElement>("a[href], button, textarea, input, select, [tabindex]"),
  ].filter(
    (element) =>
      element.tabIndex >= 0 &&
      !element.hasAttribute("disabled") &&
      element.closest("[inert], [hidden]") === null
  );
}

test("the region is named and follows the main column in the tab order", async () => {
  const user = userEvent.setup();
  const { client, view } = await drawn({ threads: [ON_STATEMENT] }, "conversation");
  try {
    const column = await columnDrawn(view, "no region is named by the margin's words");
    expect(column.tagName, "the region is no aside").toBe("ASIDE");
    const main = mainColumn(view);
    expect(main.contains(column), "the region is inside the main column").toBe(false);
    const mainStops = tabStopsIn(main);
    const columnStops = tabStopsIn(column);
    expect(mainStops.length, "the main column holds no tab stop").toBeGreaterThan(0);
    expect(columnStops.length, "the region holds no tab stop").toBeGreaterThan(0);
    expect(
      tabStopsIn(document.body).filter((element) => element.tabIndex > 0),
      "a control takes a place of its own in the tab order"
    ).toEqual([]);
    // Tab from the main column's last stop reaches the region's first.
    await act(async () => mainStops.at(-1)?.focus());
    await user.tab();
    expect(document.activeElement, "Tab from the main column's last stop").toBe(
      columnStops[0] ?? null
    );
  } finally {
    view.finish();
    client.clear();
  }
});

test("Escape in a thread focuses its node's row", async () => {
  const user = userEvent.setup();
  const { client, view } = await drawn({ threads: [ON_STATEMENT] }, "conversation");
  try {
    const column = await columnDrawn(view, "no column is drawn");
    const card = column.querySelector<HTMLElement>(
      `[data-margin-item="${CSS.escape(ON_STATEMENT.comment.id)}"]`
    );
    const control = [...(card?.querySelectorAll<HTMLElement>("button") ?? [])].find(
      (button) => button.textContent === "Resolve"
    );
    expect(control, "the statement's thread draws no Resolve").not.toBeUndefined();
    await act(async () => control?.focus());
    expect(document.activeElement).toBe(control ?? null);
    await user.keyboard("{Escape}");
    expect(document.activeElement, "Escape left the focus off the thread's row").toBe(
      rowOf(view, STATEMENT_NODE)
    );
  } finally {
    view.finish();
    client.clear();
  }
});
