import { afterEach, beforeAll, beforeEach, expect, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { act, type RenderResult, render, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import { artifactsBySlug } from "../issue-artifacts";
import type { TreeNode } from "../issue-tree";
import { NoTooltips } from "../NoTooltips";
import { NO_FAILED_READS } from "../outline-reads";
import { DECISIONS_WORDS, EMPTY_STATE_LINES } from "../page-words";
import { presetView } from "../presets";
import { Publisher, publishedTree } from "../projection-fixture";
import {
  BAND_PX,
  IssueScope,
  idsOf,
  type LayoutByHand,
  layoutByHand,
  SCREEN_PX,
  type TasksByHand,
  tasksByHand,
} from "../slices-fixtures";
import { AppProviders, focusedRow } from "../slots/stage-fixtures";
import { DecisionsFilterBody } from "./DecisionsFilterBody";
import {
  decision,
  decisionNode,
  decisionsRead,
  issueDetails,
  issueTree,
  KEY,
  openAsk,
  READER,
} from "./decisions-fixtures";

/**
 * The Decisions view's list drawn in slices (`drawn-in-slices.ts`), the plan's state table "The
 * rows drawn in slices" rows 4, 5, 6 and 10 for the list's keys, its address and its foot's
 * control: `j` on the last entry drawn and `k` on the first draw the next and the one before in
 * the same commit and move the focus there (`useTreeKeymap.ts`); an address that names an entry
 * not drawn draws every entry up to it, and it takes the focus under the header; and a press on
 * the foot's control draws the agents' decisions behind it, the first screen of them at once and
 * the rest in slices. A part's heading is drawn with its first entry drawn.
 *
 * happy-dom lays nothing out, so the page is laid out by hand, each row 100 px tall in a window
 * 600 px tall under a header's band (`layoutByHand`), and the slices are tasks run by hand
 * (`tasksByHand`).
 */

beforeAll(async () => {
  await warmMarkdownRenderer();
});

let layout: LayoutByHand;
let tasks: TasksByHand;
beforeEach(() => {
  layout = layoutByHand({ band: true });
  tasks = tasksByHand();
});
afterEach(() => {
  tasks.restore();
  layout.restore();
});

/** `count` open questions that wait on the reader, `q00` on. */
function questionsOf(count: number): TreeNode[] {
  return idsOf(count, "q").map((id) => openAsk(id));
}

/** The Decisions view of the issue holding `children`, with the decisions read answering `made`,
 *  every read answered, and `reveal` the node an address names; `redraw` draws it again over
 *  another address. */
function drawList(
  children: readonly TreeNode[],
  made: Parameters<typeof decisionsRead>[0] = [],
  reveal?: string
): { readonly redraw: (named?: string) => void; readonly view: RenderResult } {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(["decisions", KEY], decisionsRead(made));
  client.setQueryData(["asks", KEY, "thread"], []);
  client.setQueryData(["comments", KEY], []);
  const issue = issueDetails();
  const root = issueTree(children);
  const published = publishedTree(root);
  const view = presetView(root, "decisions", "", new Set()).root;
  const draw = (named: string | undefined) => (
    <MemoryRouter>
      <AppProviders client={client}>
        <Publisher published={published} />
        <IssueScope />
        <NoTooltips>
          <DecisionsFilterBody
            artifacts={artifactsBySlug(issue)}
            emptyLine={EMPTY_STATE_LINES.decisions}
            holdsEscape={false}
            isClosed={false}
            issue={issue}
            namesLine={NO_FAILED_READS}
            published={published}
            reader={READER}
            {...(named === undefined ? {} : { reveal: named })}
            root={root}
            view={view}
          />
        </NoTooltips>
      </AppProviders>
    </MemoryRouter>
  );
  const drawn = render(draw(reveal));
  return { redraw: (named) => drawn.rerender(draw(named)), view: drawn };
}

/** The entries the list draws in `container`, by their node ids, in order. */
function entriesDrawn(container: HTMLElement): string[] {
  return [...container.querySelectorAll<HTMLElement>("[data-decisions-body] [data-node-row]")].map(
    (row) => row.dataset.nodeId ?? ""
  );
}

// Row 4. `j` on the last entry drawn draws the next in the same commit and moves the focus there.
test("row 4: j on the last entry drawn draws the next and moves the focus there", async () => {
  const user = userEvent.setup();
  const questions = questionsOf(40);
  const ids = questions.map((one) => one.id);
  const { view } = drawList(questions);
  try {
    await waitFor(() => expect(entriesDrawn(view.container)).toContain(ids[0]));
    const first = entriesDrawn(view.container);
    expect(first, "the first drawing drew every entry").not.toContain(ids[39]);
    await user.keyboard("j");
    expect(focusedRow(), "j from the page did not go to the first entry").toBe(ids[0]);
    for (let at = 1; at < first.length; at += 1) await user.keyboard("j");
    const last = first.at(-1) ?? "";
    expect(focusedRow(), "j did not reach the last entry drawn").toBe(last);
    const next = ids[ids.indexOf(last) + 1] ?? "";
    expect(entriesDrawn(view.container), "the next entry was drawn already").not.toContain(next);
    await user.keyboard("j");
    expect(focusedRow(), "j on the last entry drawn did not move to the next").toBe(next);
    expect(tasks.pending(), "the entries were all drawn by then").toBe(1);
  } finally {
    view.unmount();
  }
});

// Row 5. `k` on the first entry drawn, with entries above it not drawn, draws the one above in the
// same commit and moves the focus there.
test("row 5: k on the first entry drawn, with entries above it not drawn, draws the one above", async () => {
  const user = userEvent.setup();
  const questions = questionsOf(40);
  const ids = questions.map((one) => one.id);
  const named = ids[30] ?? "";
  const { view } = drawList(questions, [], named);
  try {
    await waitFor(() => expect(focusedRow(), "the address landed nowhere").toBe(named));
    const first = entriesDrawn(view.container);
    expect(first, "the first drawing drew the first entry").not.toContain(ids[0]);
    for (let at = first.indexOf(named); at > 0; at -= 1) await user.keyboard("k");
    const top = first[0] ?? "";
    expect(focusedRow(), "k did not reach the first entry drawn").toBe(top);
    const above = ids[ids.indexOf(top) - 1] ?? "";
    await user.keyboard("k");
    expect(focusedRow(), "k on the first entry drawn did not move to the one above").toBe(above);
    expect(tasks.pending(), "the entries were all drawn by then").toBe(1);
  } finally {
    view.unmount();
  }
});

// Row 6. An address that names an entry not drawn draws every entry up to it in one commit, and
// the entry takes the focus under the header.
test("row 6: an address that names an entry not drawn draws every entry up to it and lands on it", async () => {
  const questions = questionsOf(40);
  const ids = questions.map((one) => one.id);
  const named = ids[30] ?? "";
  const list = drawList(questions);
  const { view } = list;
  try {
    await waitFor(() => expect(entriesDrawn(view.container)).toContain(ids[0]));
    expect(entriesDrawn(view.container), "the named entry was drawn at once").not.toContain(named);
    act(() => list.redraw(named));
    expect(
      entriesDrawn(view.container).slice(0, 31),
      "the address drew the entries up to its own with one left out"
    ).toEqual(ids.slice(0, 31));
    await waitFor(() => expect(focusedRow(), "the named entry took no focus").toBe(named));
    const top = layout.topOf(
      view.container.querySelector(`[data-node-row][data-node-id="${CSS.escape(named)}"]`)
    );
    expect(
      top !== undefined && top >= BAND_PX && top < SCREEN_PX,
      `the named entry stands at ${top} px, not under the header`
    ).toBe(true);
    expect(tasks.pending(), "the entries were all drawn by then").toBe(1);
  } finally {
    view.unmount();
  }
});

// Row 10. A press on the foot's control draws the agents' decisions behind it, the first screen of
// them in the press's commit and the rest in slices; none is drawn before the press.
test("row 10: the foot's control draws its decisions, the first screen first, and none before its press", async () => {
  const user = userEvent.setup();
  const questions = questionsOf(5);
  const blocks = idsOf(60, "d");
  const decided = blocks.map((block) => decisionNode(block));
  const { view } = drawList(
    [...questions, ...decided],
    blocks.map((block) => decision(block))
  );
  try {
    await waitFor(() => expect(entriesDrawn(view.container)).toContain(questions[0]?.id));
    await tasks.runAll();
    expect(
      entriesDrawn(view.container),
      "a decision of the foot was drawn before its press"
    ).toEqual(questions.map((one) => one.id));
    const control = view.getByRole("button", {
      name: (name) => name.endsWith(DECISIONS_WORDS.agents_many),
    });
    await user.click(control);
    const pressed = entriesDrawn(view.container);
    expect(pressed, "the press drew no decision of the foot at once").toContain(decided[0]?.id);
    expect(pressed, "the press drew the foot's decisions whole at once").not.toContain(
      decided.at(-1)?.id
    );
    expect(tasks.pending(), "the press asked for no slice").toBe(1);
    await tasks.runAll();
    expect(entriesDrawn(view.container), "the slices left decisions of the foot not drawn").toEqual(
      [...questions, ...decided].map((one) => one.id)
    );
  } finally {
    view.unmount();
  }
});
