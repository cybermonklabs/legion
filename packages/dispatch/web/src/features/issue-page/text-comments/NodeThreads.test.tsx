import { afterEach, beforeAll, expect, test } from "bun:test";
import type { QueryClient } from "@tanstack/react-query";
import { act, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useRef, useState } from "react";

import type { Anchor } from "../../../api/types";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import { useKeymapScope } from "../../shell/keymap";
import { node, SPEC_DOCUMENT } from "../decisions-filter/decisions-fixtures";
import { readComment } from "../outline-fixtures";
import { blockNodeId } from "../tree-drafts";
import { useTreeKeymap } from "../useTreeKeymap";
import { TEXT_THREADS_ATTRIBUTE } from "./attributes";
import {
  answerSeededReads,
  clearSelection,
  type DrawnPage,
  drawFrame,
  newClient,
  ON_STATEMENT,
  REPLY_ON_STATEMENT,
  type SeededReads,
  STATEMENT,
  STATEMENT_WORDS,
  selectWords,
  type TextThreadSeed,
} from "./margin-fixtures";
import { NodeThreads } from "./NodeThreads";
import { SelectionComment } from "./SelectionComment";
import { TEXT_COMPOSER_ATTRIBUTE } from "./TextMargin";

/**
 * R3: below 1024 px each thread on text stands in a sheet inside its node's row, after the row's
 * text, facts and acts and before its children's rows, in the order of its words; nothing folds
 * it; and `f` reaches its links and its controls as it reaches the row's own. No row site mounts
 * the sheet before its part is on the base (G6), so each case draws a row of its own as a row
 * site will place the sheet - inside the row after its words, facts and acts, outside the element
 * that names the row, its children's rows after it - in a tree the page's keys reach, on the page
 * that holds the reads the sheet draws from.
 */

/** Where the sheet stands, below 1024 px. */
const BELOW_PANE = 1000;

/** The statement's node, by its tree id, as the case's row draws it. */
const STATEMENT_NODE = blockNodeId(SPEC_DOCUMENT.slug, STATEMENT);

let stubs: { mockRestore(): void }[] = [];
beforeAll(async () => {
  await warmMarkdownRenderer();
});
afterEach(() => {
  for (const stub of stubs) stub.mockRestore();
  stubs = [];
});

/** A comment of alice's on the words `quote` of the statement, whose body is `body`. */
function onStatement(id: string, quote: string, at: number, body?: string): TextThreadSeed {
  const anchor: Anchor = { ...(ON_STATEMENT.comment.anchor as Anchor), quote };
  return {
    ...ON_STATEMENT,
    comment: { ...readComment(id, at), anchor, ...(body === undefined ? {} : { body }) },
    quote,
  };
}

/** What a case holds of the row it drew: its fold, which the case sets. */
interface RowHold {
  fold?: (folded: boolean) => void;
}

/** Nothing: what the tree's keys ask of the case's tree that no case presses. */
function nothing(): void {}

/**
 * The statement's row as a row site draws it, in a tree the page's keys reach (`useTreeKeymap`):
 * its words, with a link, which name the row; its facts; its acts, a control no key presses; the
 * sheet; and, while the row is unfolded, its child's row after it, as the outline draws its rows.
 */
function SheetRow({
  hold,
  sheet = true,
}: {
  readonly hold: RowHold;
  /** Whether the row site mounts the sheet: none does before its part is on the base (G6). */
  readonly sheet?: boolean;
}): ReactNode {
  const tree = useRef<HTMLDivElement>(null);
  const [folded, setFolded] = useState(false);
  hold.fold = setFolded;
  const statement = node({ id: STATEMENT_NODE, text: STATEMENT_WORDS, type: "requirement" });
  useKeymapScope("issue");
  useTreeKeymap({
    container: tree,
    editing: false,
    holdsEscape: false,
    onCycle: nothing,
    onEdit: nothing,
    onEditEnd: nothing,
    onFold: nothing,
    onReport: nothing,
    onReveal: nothing,
    onZoom: nothing,
    readsHeld: true,
    root: statement,
    rows: [],
    writer: undefined,
    zoom: undefined,
  });
  return (
    <div aria-label="The case's tree" data-case-tree="" ref={tree} role="tree">
      <div
        aria-expanded={!folded}
        aria-labelledby="statement-words"
        data-node-id={STATEMENT_NODE}
        data-node-row=""
        role="treeitem"
        tabIndex={0}
      >
        <div data-node-words="" id="statement-words">
          <p>
            {STATEMENT_WORDS} See <a href="https://example.com/spec">the spec</a>.
          </p>
        </div>
        {folded ? null : <p data-row-facts="">Checks serving it: 1</p>}
        <p data-row-acts="">
          <button tabIndex={-1} type="button">
            Copy reference
          </button>
        </p>
        {sheet ? <NodeThreads node={statement} /> : null}
      </div>
      {folded ? null : (
        <div data-node-id="child" data-node-row="" role="treeitem" tabIndex={-1}>
          The check under it.
        </div>
      )}
    </div>
  );
}

/** The case's row and its sheet below 1024 px, once the sheet draws `bodies`. */
async function drawnSheet(
  reads: SeededReads,
  bodies: readonly string[]
): Promise<{
  readonly client: QueryClient;
  readonly hold: RowHold;
  readonly row: HTMLElement;
  readonly sheet: HTMLElement;
  readonly view: DrawnPage;
}> {
  stubs = answerSeededReads(reads);
  const client = newClient();
  const hold: RowHold = {};
  const view = drawFrame(client, "conversation", <SheetRow hold={hold} />, { width: BELOW_PANE });
  const row = await waitFor(() => {
    const drawn = view.container.querySelector<HTMLElement>(
      `[data-case-tree] [data-node-id="${STATEMENT_NODE}"]`
    );
    expect(drawn, "the case's row is not drawn").not.toBeNull();
    return drawn as HTMLElement;
  });
  const sheet = await waitFor(
    () => {
      const drawn = row.querySelector<HTMLElement>(`[${TEXT_THREADS_ATTRIBUTE}]`);
      expect(drawn, "the row draws no sheet").not.toBeNull();
      for (const body of bodies) {
        expect(drawn?.textContent, "the sheet does not hold each thread").toContain(body);
      }
      return drawn as HTMLElement;
    },
    { timeout: 4000 }
  );
  return { client, hold, row, sheet, view };
}

/** Whether `later` stands after `earlier` in the document. */
function follows(earlier: Node, later: Node): boolean {
  return (earlier.compareDocumentPosition(later) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0;
}

test("the sheet stands after the row's facts and before its children", async () => {
  // The seeds name the thread on the later words first, so the sheet's order is its own.
  const onPlan = onStatement("on-plan", "plan", 1);
  const onPage = onStatement("on-page", "page", 2);
  const { client, row, sheet, view } = await drawnSheet({ threads: [onPlan, onPage] }, [
    onPlan.comment.body,
    onPage.comment.body,
  ]);
  try {
    expect(
      within(row).getByRole("group", { name: "Comments on text" }),
      "the sheet is no group named by the margin's words"
    ).toBe(sheet);
    const words = row.querySelector("[data-node-words]") as Element;
    const facts = row.querySelector("[data-row-facts]") as Element;
    const acts = row.querySelector("[data-row-acts]") as Element;
    for (const [name, before] of [
      ["words", words],
      ["facts", facts],
      ["acts", acts],
    ] as const) {
      expect(follows(before, sheet), `the sheet stands before the row's ${name}`).toBe(true);
    }
    expect(words.contains(sheet), "the sheet is inside the words that name the row").toBe(false);
    const child = view.container.querySelector('[data-case-tree] [data-node-id="child"]');
    expect(child, "the child's row is not drawn").not.toBeNull();
    expect(follows(sheet, child as Element), "the sheet stands after the child's row").toBe(true);
    const order = [...sheet.querySelectorAll("[data-margin-item]")].map((card) =>
      card.getAttribute("data-margin-item")
    );
    expect(order, "the threads are not in the order of their words").toEqual([
      "on-page",
      "on-plan",
    ]);
  } finally {
    view.finish();
    client.clear();
  }
});

test("folding the node keeps its sheet drawn whole", async () => {
  const { client, hold, row, sheet, view } = await drawnSheet({ threads: [ON_STATEMENT] }, [
    ON_STATEMENT.comment.body,
    REPLY_ON_STATEMENT.body,
  ]);
  try {
    expect(
      view.container.querySelector('[data-case-tree] [data-node-id="child"]'),
      "the unfolded row draws no child"
    ).not.toBeNull();
    await act(async () => hold.fold?.(true));
    expect(
      view.container.querySelector('[data-case-tree] [data-node-id="child"]'),
      "the fold left the child's row drawn"
    ).toBeNull();
    const kept = row.querySelector<HTMLElement>(`[${TEXT_THREADS_ATTRIBUTE}]`);
    expect(kept, "the fold took the sheet away").toBe(sheet);
    for (const body of [ON_STATEMENT.comment.body, REPLY_ON_STATEMENT.body]) {
      expect(kept?.textContent, "the fold cut the thread").toContain(body);
    }
    const card = kept?.querySelector<HTMLElement>(
      `[data-margin-item="${ON_STATEMENT.comment.id}"]`
    );
    expect(card?.getAttribute("aria-expanded"), "the thread's card is collapsed").toBe("true");
    expect(
      kept?.querySelector('button[aria-expanded="false"]'),
      "the sheet draws a collapsed preview"
    ).toBeNull();
  } finally {
    view.finish();
    client.clear();
  }
});

test("f reaches a sheet thread's links, then its controls", async () => {
  const user = userEvent.setup();
  const linked = onStatement(
    "on-linked",
    "page",
    1,
    "Read [the notes](https://example.com/notes) first."
  );
  const { client, row, sheet, view } = await drawnSheet({ threads: [linked] }, ["the notes"]);
  try {
    const sheetLinks = await waitFor(() => {
      const links = [...sheet.querySelectorAll<HTMLElement>("a[href]")];
      expect(links.length, "the sheet draws no link").toBeGreaterThan(0);
      return links;
    });
    const sheetControls = [...sheet.querySelectorAll<HTMLButtonElement>("button")].filter(
      (control) => !control.disabled
    );
    expect(sheetControls.length, "the sheet draws no control").toBeGreaterThan(0);
    const rowLink = row.querySelector<HTMLElement>("[data-node-words] a[href]") as HTMLElement;
    const rowControl = row.querySelector<HTMLElement>("[data-row-acts] button") as HTMLElement;

    // Each `f` from the row, until it comes round to the first stop again.
    await act(async () => row.focus());
    const stops: Element[] = [];
    for (let press = 0; press < 40; press += 1) {
      await user.keyboard("f");
      const now = document.activeElement;
      if (now === null || now === stops[0]) break;
      stops.push(now);
    }
    const at = (element: Element) => stops.indexOf(element);
    expect(at(rowLink), "f does not reach the row's own link").toBe(0);
    for (const link of sheetLinks) {
      expect(at(link), "f does not reach the sheet's link next after the row's").toBeGreaterThan(0);
      expect(at(link), "f reaches the sheet's link after a control").toBeLessThan(at(rowControl));
    }
    expect(at(rowControl), "f does not reach the row's control").toBeGreaterThan(0);
    const reached = sheetControls.filter((control) => at(control) !== -1);
    expect(reached.length, "f reaches none of the sheet's controls").toBeGreaterThan(0);
    for (const control of reached) {
      expect(at(control), "f reaches a sheet's control before the row's").toBeGreaterThan(
        at(rowControl)
      );
    }
  } finally {
    view.finish();
    client.clear();
  }
});

/** The case's row, with the control "Comment" beside it, below 1024 px; a sheet under the row
 *  where `sheet` holds. */
async function drawnForControl(
  sheet: boolean
): Promise<{ readonly client: QueryClient; readonly row: HTMLElement; readonly view: DrawnPage }> {
  stubs = answerSeededReads({ threads: [] });
  const client = newClient();
  const hold: RowHold = {};
  const view = drawFrame(
    client,
    "conversation",
    <>
      <SheetRow hold={hold} sheet={sheet} />
      <SelectionComment />
    </>,
    { width: BELOW_PANE }
  );
  const row = await waitFor(() => {
    const drawn = view.container.querySelector<HTMLElement>(
      `[data-case-tree] [data-node-id="${STATEMENT_NODE}"]`
    );
    expect(drawn, "the case's row is not drawn").not.toBeNull();
    return drawn as HTMLElement;
  });
  return { client, row, view };
}

/** The animation frames the control reads a selection in, passed. */
async function framesPass(): Promise<void> {
  for (let frame = 0; frame < 3; frame += 1) {
    await act(async () => {
      await new Promise((done) => requestAnimationFrame(() => done(undefined)));
    });
  }
}

// R5: below 1024 px the composer stands in the node's sheet, so a control stands only where a row
// site mounts one. Where none does (until G6) it would promise a write the page cannot open.
test("below 1024 px with the row's sheet drawn, a selection draws the control and pressing it opens the composer in the sheet", async () => {
  const user = userEvent.setup();
  const { client, row, view } = await drawnForControl(true);
  try {
    selectWords(row, "page");
    const control = await waitFor(() => view.getByRole("button", { name: "Comment" }));
    await user.click(control);
    await waitFor(() =>
      expect(
        row.querySelector(`[${TEXT_THREADS_ATTRIBUTE}] [${TEXT_COMPOSER_ATTRIBUTE}]`),
        "the sheet holds no composer"
      ).not.toBeNull()
    );
  } finally {
    await act(async () => clearSelection());
    view.finish();
    client.clear();
  }
});

test("below 1024 px with no sheet under the row, a selection draws no control and the chord opens no composer, and from 1024 px the control stands", async () => {
  const user = userEvent.setup();
  const { client, row, view } = await drawnForControl(false);
  try {
    selectWords(row, "page");
    await framesPass();
    expect(
      view.queryByRole("button", { name: "Comment" }),
      "a control stands where no sheet takes its composer"
    ).toBeNull();
    await user.keyboard("{Control>}{Alt>}m{/Alt}{/Control}");
    await framesPass();
    expect(
      view.container.querySelector(`[${TEXT_COMPOSER_ATTRIBUTE}]`),
      "the chord opened a composer with no place"
    ).toBeNull();
    await act(async () => view.window.set(1440));
    await waitFor(() =>
      expect(
        view.queryByRole("button", { name: "Comment" }),
        "the control does not stand where the column takes the composer"
      ).not.toBeNull()
    );
  } finally {
    await act(async () => clearSelection());
    view.finish();
    client.clear();
  }
});
