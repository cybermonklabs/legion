import { afterEach, expect, type Mock, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { act, type RenderResult, waitFor } from "@testing-library/react";
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
  ON_CHECK,
  ON_DECISION,
  ON_STATEMENT,
  REPLY_ON_STATEMENT,
  readAgain,
  rowOf,
  SERVES,
  type SeededReads,
  STATEMENT,
  seededNodes,
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

/** A client as the page's tests take one: no read tried again. */
function newClient(): QueryClient {
  return new QueryClient({ defaultOptions: { queries: { retry: false } } });
}

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
 *  selection does (`SelectionComment`, t8). */
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

// R2: the stacking of the column waits for pull request 3125 (G5): the thread of the focused row
// stands level with its words, the threads above it move up and those under it move down.
test.skipIf(!GATES.COMPOSER)(
  "column, j or k moves the focus to a row with threads: that row's first thread stands level with its words, the threads above move up and those below move down",
  async () => {
    const user = userEvent.setup();
    const { client, view } = await drawn({ threads: [ON_STATEMENT, ON_CHECK] }, "conversation");
    try {
      const column = await columnDrawn(view, "no column is drawn");
      const words = (id: string): Element | null =>
        rowOf(view, id)?.querySelector("[data-node-words]") ?? null;
      const wanted = new Map<Element | null, number>([
        [words(STATEMENT_NODE), 100],
        [words(blockNodeId("verification-md", "c1")), 300],
      ]);
      const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
        this: Element
      ) {
        return new DOMRect(0, wanted.get(this) ?? 0, 400, 24);
      });
      stubs.push(rect);
      await readAgain(client);
      rowOf(view, STATEMENT_NODE)?.focus();
      await user.keyboard("j");
      const tops = cardsOf(column).map((card) => Number.parseFloat(card.style.top));
      expect(tops[0], "the first thread did not move up for the focused row's").toBeLessThanOrEqual(
        100
      );
      expect(tops[1], "the focused row's thread is not level with its words").toBe(300);
    } finally {
      view.finish();
      client.clear();
    }
  }
);

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
