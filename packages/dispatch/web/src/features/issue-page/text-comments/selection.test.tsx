import { afterEach, beforeAll, expect, spyOn, test } from "bun:test";
import type { QueryClient } from "@tanstack/react-query";
import { act, type RenderResult, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";

import { ApiError, api } from "../../../api/client";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import { SPEC_DOCUMENT } from "../decisions-filter/decisions-fixtures";
import { readComment } from "../outline-fixtures";
import { blockNodeId } from "../tree-drafts";
import { GATES } from "./gates";
import {
  answerSeededReads,
  clearSelection,
  columnOf,
  type DrawnPage,
  drawFrame,
  newClient,
  ON_STATEMENT,
  OTHER,
  OTHER_WORDS,
  PLAN_DOCUMENT,
  readAgain,
  rectOf,
  rowOf,
  type SeededReads,
  STATEMENT,
  selectAcross,
  selectWords,
  setSelection,
} from "./margin-fixtures";
import { SelectionComment } from "./SelectionComment";
import { TEXT_COMPOSER_ATTRIBUTE } from "./TextMargin";

/**
 * R5: a selection in the words of one row, its control, and the composer it opens. Each case is a
 * row of the plan's table, and each that asserts the control or the composer away first asserts it
 * drawn, so a control that is never drawn fails the row on its own words. The control stands
 * `position: fixed` at the last client rectangle of the selection's range, which happy-dom lays
 * out nowhere, so a case gives the range its rectangle. The send rows wait for pull request 3125,
 * which brings the composer's fields (G5).
 */

/** The control's name and the margin's name for its column (`page-words-margin.ts`, R11). */
const CONTROL = "Comment";
const COLUMN = "Comments on text";
const NO_LONGER = "The selected words are no longer in this text. Select them again.";
const NOT_KEPT = "The selected words were not kept, so this reply stands under what it answers.";

/** The statement's row, which the selection is in, and the check's, under it. */
const STATEMENT_NODE = blockNodeId(SPEC_DOCUMENT.slug, STATEMENT);
const OTHER_NODE = blockNodeId(PLAN_DOCUMENT.slug, OTHER);
const CHECK_NODE = blockNodeId("verification-md", "c1");

let stubs: { mockRestore(): void }[] = [];
let rangeStubs: { mockRestore(): void }[] = [];
beforeAll(async () => {
  await warmMarkdownRenderer();
});
afterEach(() => {
  clearSelection();
  for (const stub of [...rangeStubs, ...stubs]) stub.mockRestore();
  rangeStubs = [];
  stubs = [];
});

/** The page as `drawFrame` draws it with the control beside it, `reads` answered, once the
 *  statement's row is drawn. */
async function drawn(
  reads: SeededReads,
  options: { drawer?: boolean } = {}
): Promise<{ readonly client: QueryClient; readonly row: HTMLElement; readonly view: DrawnPage }> {
  stubs = answerSeededReads(reads);
  const client = newClient();
  const view = drawFrame(client, "conversation", <SelectionComment />, options);
  const row = await waitFor(() => {
    const found = rowOf(view, STATEMENT_NODE);
    expect(found, "the statement's row is not drawn").not.toBeNull();
    return found as HTMLElement;
  });
  return { client, row, view };
}

/** Gives the range of every selection `rect` as its last client rectangle. */
function rangeEndsAt(rect: DOMRect): void {
  for (const stub of rangeStubs) stub.mockRestore();
  const list = Object.assign([rect], { item: (index: number) => [rect][index] ?? null });
  rangeStubs = [
    spyOn(Range.prototype, "getClientRects").mockReturnValue(list as unknown as DOMRectList),
    spyOn(Range.prototype, "getBoundingClientRect").mockReturnValue(rect),
  ];
}

/** The control, where it is drawn. */
function controlOf(view: RenderResult): HTMLElement | null {
  return view.queryByRole("button", { name: CONTROL });
}

/** The control, drawn, or a failure that says it is not. */
async function controlDrawn(view: RenderResult, said: string): Promise<HTMLElement> {
  return waitFor(
    () => {
      const control = controlOf(view);
      expect(control, said).not.toBeNull();
      return control as HTMLElement;
    },
    { timeout: 1500 }
  );
}

/** Waits for the control to go. */
async function controlGone(view: RenderResult, said: string): Promise<void> {
  await waitFor(() => expect(controlOf(view), said).toBeNull(), { timeout: 1500 });
}

/** Where the control stands, in px, from the `left` and `top` of the box that is fixed. */
function placeOf(control: HTMLElement): { readonly left: number; readonly top: number } {
  const box = control.closest<HTMLElement>('[style*="left"]') ?? control;
  return { left: Number.parseFloat(box.style.left), top: Number.parseFloat(box.style.top) };
}

/** Presses the chord `$mod+Alt+m`, matched by `event.code === "KeyM"`. */
async function pressChord(user: UserEvent): Promise<void> {
  await user.keyboard("{Control>}{Alt>}m{/Alt}{/Control}");
}

/** The composer, in the column, once it is drawn. */
async function composerIn(view: RenderResult, said: string): Promise<HTMLElement> {
  const column = await columnOf(view, COLUMN);
  expect(column, `${said}: no column is drawn`).not.toBeNull();
  return waitFor(() => {
    const composer = (column as HTMLElement).querySelector<HTMLElement>(
      `[${TEXT_COMPOSER_ATTRIBUTE}]`
    );
    expect(composer, `${said}: the column holds no composer`).not.toBeNull();
    return composer as HTMLElement;
  });
}

/** Selects "page" in the statement, presses the control, and answers the composer it opens. */
async function openComposer(
  view: RenderResult,
  row: HTMLElement,
  user: UserEvent
): Promise<HTMLElement> {
  rangeEndsAt(rectOf(100, 200, 80, 20));
  selectWords(row, "page");
  await user.click(
    await controlDrawn(view, "a selection in the statement's words draws no control")
  );
  return composerIn(view, "pressing the control");
}

test("no selection, a selection inside the words of one row: the control is drawn beside the selection's end", async () => {
  const { client, row, view } = await drawn({ threads: [] });
  try {
    rangeEndsAt(rectOf(100, 200, 80, 20));
    selectWords(row, "page");
    const control = await controlDrawn(view, "a selection in one row's words draws no control");
    const { left, top } = placeOf(control);
    expect(left, "the control is left of the selection's end").toBeGreaterThanOrEqual(180);
    expect(top, "the control is not at the selection's last line").toBeGreaterThanOrEqual(200 - 24);
    expect(top, "the control is not at the selection's last line").toBeLessThanOrEqual(220 + 24);
  } finally {
    view.finish();
    client.clear();
  }
});

test("no selection, a selection across two rows, two blocks of one item, or into the row's margin: no control", async () => {
  stubs = answerSeededReads({ threads: [] });
  const client = newClient();
  const view = drawFrame(client, "conversation", <SelectionComment />);
  try {
    const statement = await waitFor(() => {
      const found = rowOf(view, STATEMENT_NODE);
      expect(found).not.toBeNull();
      return found as HTMLElement;
    });
    const check = await waitFor(() => {
      const found = rowOf(view, CHECK_NODE);
      expect(found).not.toBeNull();
      return found as HTMLElement;
    });
    const other = await waitFor(() => {
      const found = rowOf(view, OTHER_NODE);
      expect(found).not.toBeNull();
      return found as HTMLElement;
    });
    rangeEndsAt(rectOf(100, 200, 80, 20));
    // The control a selection inside one row's words draws, which each of the three below does not.
    selectWords(statement, "page");
    await controlDrawn(view, "a selection in one row's words draws no control");
    selectAcross(statement, "page", check, "draws");
    await controlGone(view, "a selection across two rows drew a control");
    selectWords(statement, "page");
    await controlDrawn(view, "the control did not return for one row's words");
    selectAcross(other, OTHER_WORDS, other, "second block");
    await controlGone(view, "a selection across two blocks of one item drew a control");
    selectWords(statement, "page");
    await controlDrawn(view, "the control did not return for one row's words again");
    const margin = statement.querySelector<HTMLElement>("[data-node-margin]");
    expect(margin, "the statement's row draws no margin").not.toBeNull();
    const walker = document.createTreeWalker(margin as HTMLElement, NodeFilter.SHOW_TEXT);
    const inMargin = walker.nextNode() as Text | null;
    expect(inMargin, "the margin draws no text").not.toBeNull();
    const words = statement.querySelector("[data-node-words]")?.firstChild?.firstChild as Text;
    const range = document.createRange();
    range.setStart(words, 0);
    range.setEnd(inMargin as Text, 1);
    setSelection(range);
    await controlGone(view, "a selection that reaches the row's margin drew a control");
  } finally {
    view.finish();
    client.clear();
  }
});

test("control shown, the selection changes inside the same words: the control moves to the new end", async () => {
  const { client, row, view } = await drawn({ threads: [] });
  try {
    rangeEndsAt(rectOf(100, 200, 80, 20));
    selectWords(row, "page");
    const first = placeOf(await controlDrawn(view, "a selection draws no control"));
    rangeEndsAt(rectOf(100, 260, 160, 20));
    selectWords(row, "page draws");
    await waitFor(() => {
      const control = controlOf(view);
      expect(control, "the control went with the selection's change").not.toBeNull();
      const moved = placeOf(control as HTMLElement);
      expect(moved.top, "the control did not move to the new end's line").toBeGreaterThan(
        first.top
      );
      expect(moved.left, "the control did not move to the new end").toBeGreaterThan(first.left);
    });
  } finally {
    view.finish();
    client.clear();
  }
});

test("control shown, the selection collapses or leaves the words: no control", async () => {
  const { client, row, view } = await drawn({ threads: [] });
  const outside = document.createElement("p");
  outside.textContent = "Words outside the stage.";
  document.body.append(outside);
  try {
    rangeEndsAt(rectOf(100, 200, 80, 20));
    selectWords(row, "page");
    await controlDrawn(view, "a selection draws no control");
    clearSelection();
    await controlGone(view, "the control stays once the selection collapses");
    selectWords(row, "page");
    await controlDrawn(view, "the control did not return");
    const range = document.createRange();
    range.selectNodeContents(outside);
    setSelection(range);
    await controlGone(view, "the control stays once the selection leaves the words");
  } finally {
    outside.remove();
    view.finish();
    client.clear();
  }
});

test("control shown, press the control or $mod+Alt+m: the composer opens in the column beside the node, headed by the quote, and the column is drawn if it was not", async () => {
  for (const how of ["the control", "the chord"] as const) {
    const user = userEvent.setup();
    const { client, row, view } = await drawn({ threads: [] });
    try {
      expect(await columnOf(view, COLUMN, 300), "a column is drawn with no thread").toBeNull();
      rangeEndsAt(rectOf(100, 200, 80, 20));
      selectWords(row, "page");
      const control = await controlDrawn(view, "a selection draws no control");
      if (how === "the control") await user.click(control);
      else await pressChord(user);
      const composer = await composerIn(view, how);
      expect(composer.textContent, `the composer is not headed by the quote (${how})`).toContain(
        "page"
      );
      expect(
        within(composer).queryByRole("textbox"),
        `the composer has no box to write in (${how})`
      ).not.toBeNull();
    } finally {
      view.finish();
      client.clear();
    }
  }
});

test("no selection, $mod+Alt+m: nothing is written and nothing is drawn", async () => {
  const user = userEvent.setup();
  const create = spyOn(api, "createComment");
  const { client, row, view } = await drawn({ threads: [] });
  stubs.push(create);
  try {
    await pressChord(user);
    expect(create, "the chord with no selection wrote a comment").not.toHaveBeenCalled();
    expect(controlOf(view), "the chord with no selection drew a control").toBeNull();
    expect(
      await columnOf(view, COLUMN, 300),
      "the chord with no selection drew a column"
    ).toBeNull();
    // The chord with a selection, which the case above leaves out, opens the composer.
    rangeEndsAt(rectOf(100, 200, 80, 20));
    selectWords(row, "page");
    await pressChord(user);
    await composerIn(view, "the chord with a selection");
  } finally {
    view.finish();
    client.clear();
  }
});

// The send rows wait for the composer's fields of pull request 3125 (G5): R5's anchor and reply
// fields in `composer-model.ts`, `send-request.ts` and `held-sends.ts`.

/** Types `draft` in the composer's box and presses Send. */
async function send(
  composer: HTMLElement,
  user: UserEvent,
  draft = "Why this word?"
): Promise<void> {
  await user.type(within(composer).getByRole("textbox"), draft);
  await user.click(composer.querySelector<HTMLElement>('button[type="submit"]') as HTMLElement);
}

test.skipIf(!GATES.COMPOSER)(
  "composer open, Send: sending, the draft is held and Send shows its pending state",
  async () => {
    const user = userEvent.setup();
    const pending = Promise.withResolvers<never>();
    const { client, row, view } = await drawn({ threads: [] });
    const create = spyOn(api, "createComment").mockReturnValue(pending.promise);
    stubs.push(create);
    try {
      const composer = await openComposer(view, row, user);
      await send(composer, user);
      await waitFor(() => expect(create).toHaveBeenCalledTimes(1));
      expect(create.mock.calls[0]?.[1], "the send's anchor").toMatchObject({
        anchor: { artifact: SPEC_DOCUMENT.id, block_id: STATEMENT, occurrence: 0, quote: "page" },
        body: "Why this word?",
      });
      const submit = composer.querySelector<HTMLElement>('button[type="submit"]');
      expect(
        submit?.hasAttribute("disabled") || submit?.getAttribute("aria-busy") === "true",
        "Send shows no pending state"
      ).toBe(true);
      expect(within(composer).getByRole("textbox")).toHaveValue("Why this word?");
    } finally {
      view.finish();
      client.clear();
    }
  }
);

test.skipIf(!GATES.COMPOSER)(
  "sending, 201: the composer closes and the new thread stands beside its node with the focus on its root, before and after the re-read",
  async () => {
    const user = userEvent.setup();
    const reads: SeededReads = { threads: [] };
    const { client, row, view } = await drawn(reads);
    const created = { ...ON_STATEMENT.comment, id: "new-thread" };
    stubs.push(spyOn(api, "createComment").mockResolvedValue(created));
    try {
      const composer = await openComposer(view, row, user);
      reads.threads = [{ ...ON_STATEMENT, comment: created }];
      await send(composer, user);
      await waitFor(() =>
        expect(view.container.querySelector(`[${TEXT_COMPOSER_ATTRIBUTE}]`)).toBeNull()
      );
      const root = await waitFor(() => {
        const card = view
          .getByRole("region", { name: COLUMN })
          .querySelector<HTMLElement>('[data-margin-item="new-thread"]');
        expect(card, "the new thread stands in no card").not.toBeNull();
        return card as HTMLElement;
      });
      expect(root.contains(document.activeElement), "the focus is not on the thread's root").toBe(
        true
      );
      await readAgain(client);
      expect(
        root.isConnected && root.contains(document.activeElement),
        "the re-read moved the focus"
      ).toBe(true);
    } finally {
      view.finish();
      client.clear();
    }
  }
);

test.skipIf(!GATES.COMPOSER)(
  "sending, 400 INVALID_ANCHOR or 404 TARGET_NOT_FOUND: the draft stays and one sentence says the words are no longer in the text",
  async () => {
    for (const refusal of [
      new ApiError(400, { code: "INVALID_ANCHOR", error: "no such quote" }),
      new ApiError(404, { code: "TARGET_NOT_FOUND", error: "no such occurrence" }),
    ]) {
      const user = userEvent.setup();
      const { client, row, view } = await drawn({ threads: [] });
      stubs.push(spyOn(api, "createComment").mockRejectedValue(refusal));
      try {
        const composer = await openComposer(view, row, user);
        await send(composer, user);
        const status = await waitFor(() => within(composer).getByRole("status"));
        expect(status.textContent, `the sentence for ${refusal.status}`).toBe(NO_LONGER);
        expect(within(composer).getByRole("textbox"), "the draft went").toHaveValue(
          "Why this word?"
        );
      } finally {
        view.finish();
        client.clear();
      }
    }
  }
);

test.skipIf(!GATES.COMPOSER)(
  "sending, any other refusal: the draft stays and the composer's own failure words are drawn",
  async () => {
    const user = userEvent.setup();
    const { client, row, view } = await drawn({ threads: [] });
    stubs.push(
      spyOn(api, "createComment").mockRejectedValue(
        new ApiError(500, { code: "INTERNAL", error: "the write failed" })
      )
    );
    try {
      const composer = await openComposer(view, row, user);
      await send(composer, user);
      await waitFor(() => expect(composer.textContent).not.toContain(NO_LONGER));
      expect(within(composer).getByRole("textbox"), "the draft went").toHaveValue("Why this word?");
      expect(
        composer.querySelector('[role="alert"], [role="status"]'),
        "the composer says nothing of the refusal"
      ).not.toBeNull();
    } finally {
      view.finish();
      client.clear();
    }
  }
);

// A reply to a block of a post, whose 201 keeps no quote (an older build, while two builds serve
// one database): the reply is a row under what it answers (decision 465 (4)).
test.skipIf(!GATES.COMPOSER || !GATES.REPLY_QUOTE)(
  "sending a reply, 201 whose reply_block holds no quote: the composer closes, the reply is a row under what it answers, the sentence is drawn with role status, and no thread",
  async () => {
    const user = userEvent.setup();
    const post = readComment("post", 6);
    const reads: SeededReads = { posts: [post], threads: [] };
    const { client, view } = await drawn(reads);
    const reply = { ...readComment("kept-no-quote", 7, "post"), reply_block: null };
    stubs.push(spyOn(api, "createComment").mockResolvedValue(reply));
    try {
      const postRow = await waitFor(() => {
        const found = rowOf(view, "post");
        expect(found, "the post's row is not drawn").not.toBeNull();
        return found as HTMLElement;
      });
      rangeEndsAt(rectOf(100, 200, 80, 20));
      selectWords(postRow, "words");
      await user.click(await controlDrawn(view, "a selection in a post's words draws no control"));
      const composer = await composerIn(view, "pressing the control on a post");
      await send(composer, user);
      await waitFor(() =>
        expect(view.container.querySelector(`[${TEXT_COMPOSER_ATTRIBUTE}]`)).toBeNull()
      );
      await waitFor(() => expect(view.getByText(NOT_KEPT).getAttribute("role")).toBe("status"));
      expect(
        view
          .getByRole("region", { name: COLUMN })
          .querySelector('[data-margin-item="kept-no-quote"]'),
        "the reply stands in the column as a thread"
      ).toBeNull();
    } finally {
      view.finish();
      client.clear();
    }
  }
);

test("composer open, Escape: the composer closes, the draft is kept as the composer keeps one, and the focus goes to the node's row", async () => {
  const user = userEvent.setup();
  const { client, row, view } = await drawn({ threads: [] });
  try {
    const composer = await openComposer(view, row, user);
    await user.type(within(composer).getByRole("textbox"), "Why this word?");
    await user.keyboard("{Escape}");
    await waitFor(() =>
      expect(view.container.querySelector(`[${TEXT_COMPOSER_ATTRIBUTE}]`)).toBeNull()
    );
    expect(document.activeElement?.closest("[data-node-row]"), "the focus is not on the row").toBe(
      row
    );
    const again = await openComposer(view, row, user);
    expect(within(again).getByRole("textbox"), "the draft was not kept").toHaveValue(
      "Why this word?"
    );
  } finally {
    view.finish();
    client.clear();
  }
});

test("composer open, Details opens: the composer keeps its draft, hidden with the column", async () => {
  const user = userEvent.setup();
  const { client, row, view } = await drawn({ threads: [] }, { drawer: true });
  try {
    const composer = await openComposer(view, row, user);
    await user.type(within(composer).getByRole("textbox"), "Why this word?");
    await act(async () => view.held.drawer?.setOpen(true));
    const kept = view.container.querySelector<HTMLElement>(`[${TEXT_COMPOSER_ATTRIBUTE}]`);
    expect(kept, "Details took the composer away").not.toBeNull();
    expect(kept?.closest("[inert]"), "the composer is reachable under Details").not.toBeNull();
    expect(within(kept as HTMLElement).getByRole("textbox", { hidden: true })).toHaveValue(
      "Why this word?"
    );
  } finally {
    view.finish();
    client.clear();
  }
});

test("control shown, Details open, press the control or the chord: Details closes, then the composer opens in the column", async () => {
  for (const how of ["the control", "the chord"] as const) {
    const user = userEvent.setup();
    const { client, row, view } = await drawn({ threads: [] }, { drawer: true });
    try {
      await act(async () => view.held.drawer?.setOpen(true));
      rangeEndsAt(rectOf(100, 200, 80, 20));
      selectWords(row, "page");
      const control = await controlDrawn(view, "a selection with Details open draws no control");
      if (how === "the control") await user.click(control);
      else await pressChord(user);
      await waitFor(() =>
        expect(view.held.drawer?.open, `Details stays open (${how})`).toBe(false)
      );
      await composerIn(view, how);
    } finally {
      view.finish();
      client.clear();
    }
  }
});

test("composer open, the tree read is read again: the composer stays open on the same node", async () => {
  const user = userEvent.setup();
  const { client, row, view } = await drawn({ threads: [] });
  try {
    const composer = await openComposer(view, row, user);
    await user.type(within(composer).getByRole("textbox"), "Why this word?");
    await readAgain(client);
    const kept = await composerIn(view, "after the re-read");
    expect(kept.textContent, "the composer left the node it was opened on").toContain("page");
    expect(within(kept).getByRole("textbox"), "the draft went with the re-read").toHaveValue(
      "Why this word?"
    );
  } finally {
    view.finish();
    client.clear();
  }
});

test("composer open, the node's row is folded or paged away: the composer stays in the column at the node's last place", async () => {
  const user = userEvent.setup();
  const reads: SeededReads = { threads: [] };
  const { client, row, view } = await drawn(reads);
  try {
    const composer = await openComposer(view, row, user);
    await user.type(within(composer).getByRole("textbox"), "Why this word?");
    const column = await columnOf(view, COLUMN);
    const top = (
      column?.querySelector(`[${TEXT_COMPOSER_ATTRIBUTE}]`) as HTMLElement | null
    )?.closest<HTMLElement>('[style*="top"]')?.style.top;
    reads.without = [STATEMENT];
    await readAgain(client);
    await waitFor(() => expect(rowOf(view, STATEMENT_NODE), "the row is still drawn").toBeNull());
    const kept = await composerIn(view, "after the node's row went");
    expect(within(kept).getByRole("textbox"), "the draft went with the row").toHaveValue(
      "Why this word?"
    );
    expect(
      kept.closest<HTMLElement>('[style*="top"]')?.style.top,
      "the composer moved from the node's last place"
    ).toBe(top);
  } finally {
    view.finish();
    client.clear();
  }
});
