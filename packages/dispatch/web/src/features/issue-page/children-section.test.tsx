import { afterEach, expect, type Mock, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { act, type RenderResult, render, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { nextFrame } from "../../__tests__/frames";
import { api } from "../../api/client";
import type { IssueChild, IssueDetails } from "../../api/types";
import { DESKTOP_WIDTH, stubViewport } from "../margin/margin-fixture";
import { buildIssuePath, parseIssuePath } from "../refs/routes";
import { userPreferenceStorageKey } from "../shell/userPreference";
import { CHILDREN_PAGE, CHILDREN_SECTION, CHILDREN_TOGGLE } from "./children-section";
import { issueDetails, KEY } from "./decisions-filter/decisions-fixtures";
import { presetPath } from "./issue-address";
import { TWO_THREADS } from "./outline-fixtures";
import { PRESET_NAMES, PRESET_ORDER } from "./page-words";
import { ProvidersSlot } from "./slots/providers";
import { StageSlot } from "./slots/stage";
import {
  AppProviders,
  commentNodes,
  focusedRow,
  rowTakesFocus,
  stubReads,
  treeOf,
} from "./slots/stage-fixtures";
import { ViewRowSlot } from "./slots/view-row";
import { treeNode } from "./tree-read/tree-fixtures";
import type { TreeAnswer } from "./tree-read/tree-read";

/**
 * The children's section above the outline (R14): drawn in Conversation, in each filter and in
 * Graph, open on a first load, 20 children at a time, kept open or closed as the reader's own
 * preference, reached by `t h` and `/children`, and opened at a child by that child's tag on its
 * row of the outline (R9).
 */

/** Where the reader's choice is kept for the reader `stubReads` signs in. */
const KEPT = userPreferenceStorageKey("alice", "issue.children-section");

/** The mark of each slot the stage draws: the body, which draws the outline in Conversation and
 *  Graph, and each filter's own body. */
const SLOT_MARKS =
  "[data-issue-body], [data-spec-body], [data-verification-body], [data-plan-body], [data-decisions-body]";

/** `count` children of the issue, in the issue read's order, `CORE-2` first: every third one
 *  done, the first two with children of their own, so the direct count and the whole subtree's
 *  differ; the second waits on the third, and the third is held by an agent. */
function childrenOf(count: number): IssueChild[] {
  return Array.from({ length: count }, (_, at) => {
    const key = `CORE-${at + 2}`;
    const done = at % 3 === 0;
    const own = at < 2 ? 3 : 1;
    return {
      active_at: "2026-11-05T12:00:00Z",
      external_links: null,
      key,
      status: done ? "done" : "todo",
      subtree_done: done ? own : 0,
      subtree_total: own,
      title: `The child ${key}`,
      ...(at === 1 ? { needs: ["CORE-4"] } : {}),
      ...(at === 2
        ? {
            claim: {
              actor: { id: "11111111-2222-4333-8444-555555555555", kind: "session" as const },
              at: "2026-11-05T12:00:00Z",
            },
          }
        : {}),
    };
  });
}

/** The fixtures' issue with `count` children. */
function issueWith(count: number, key = KEY): IssueDetails {
  return { ...issueDetails(), children: childrenOf(count), key };
}

/** Where the router is, as the case reads it after a key or a press. */
const at: { address?: string } = {};

/** The slots of the page that hold the children's section and its chord, as the page's root
 *  draws them (`IssueOutlinePage.tsx`): the view row, whose chords include `t h`, and the stage,
 *  the route read from the address at every render. */
function Frame({ issue }: { issue: IssueDetails }): ReactNode {
  const { pathname, search } = useLocation();
  at.address = `${pathname}${search}`;
  const route = parseIssuePath(pathname, search);
  if (route === undefined) throw new Error(`${pathname} names no route`);
  return (
    <ProvidersSlot issue={issue} route={route}>
      <ViewRowSlot issue={issue} route={route} />
      <StageSlot issue={issue} route={route} />
    </ProvidersSlot>
  );
}

/** The frame of `issue` at `address`, under what the app provides above the page. */
function drawFrame(issue: IssueDetails, address: string): RenderResult {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <MemoryRouter initialEntries={[address]}>
      <AppProviders client={client}>
        <Frame issue={issue} />
      </AppProviders>
    </MemoryRouter>
  );
}

/** The section, where the page draws one. */
function section(view: RenderResult): HTMLElement | null {
  return view.container.querySelector<HTMLElement>(CHILDREN_SECTION);
}

/** The section's heading control, which opens and closes its list. */
function toggle(view: RenderResult): HTMLButtonElement {
  const found = view.container.querySelector<HTMLButtonElement>(CHILDREN_TOGGLE);
  if (found === null) throw new Error("the page drew no children's section");
  return found;
}

/** The keys of the children the section lists, in order. */
function listed(view: RenderResult): string[] {
  return [...view.container.querySelectorAll<HTMLElement>("[data-child-entry]")].map(
    (entry) => entry.dataset.childEntry ?? ""
  );
}

/** The section's control that lists the next children, where it draws one. */
function more(view: RenderResult): HTMLButtonElement | null {
  return view.container.querySelector<HTMLButtonElement>("[data-children-more]");
}

/** The key link of the child `key`'s entry in the section. */
function entryKey(view: RenderResult, key: string): HTMLElement | null {
  return (
    section(view)?.querySelector<HTMLElement>(`[data-child-entry="${key}"] [data-child-key]`) ??
    null
  );
}

/** Waits for the outline to draw the first thread's row: by then every read of the page has
 *  answered, who is reading among them, whose browser keeps the section's choice. */
async function outlineDrawn(view: RenderResult): Promise<void> {
  await waitFor(() =>
    expect(view.container.querySelector('[data-node-row][data-node-id="head-a"]')).not.toBeNull()
  );
}

/** Waits for the next frame and the one after it, where the page places a focus. */
async function frames(): Promise<void> {
  for (let left = 2; left > 0; left -= 1) {
    await act(nextFrame);
  }
}

let restore: { mockRestore(): void }[] = [];
let restoreViewport: (() => void) | undefined;
afterEach(() => {
  for (const spy of restore) spy.mockRestore();
  restore = [];
  restoreViewport?.();
  restoreViewport = undefined;
  window.localStorage.removeItem(KEPT);
});

/** The outline read's answer: the issue's two threads, then a row under the issue for each of
 *  `children`. */
function treeWith(children: readonly IssueChild[]): TreeAnswer {
  return treeOf([
    ...commentNodes(TWO_THREADS),
    ...children.map((child) => treeNode(child.key, "issue", KEY, { text: child.title })),
  ]);
}

/** Every read of the page answered, the outline read with the issue's two threads and, where
 *  `children` gives them, a row for each of those children under the issue (`treeWith`). */
function answerReads(children: readonly IssueChild[] = []): void {
  restoreViewport = stubViewport(DESKTOP_WIDTH);
  restore = stubReads();
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async () =>
    treeWith(children)
  );
}

// R14: in Conversation, in each filter and in Graph the section stands above the outline, opens
// expanded, and lists 20 children, each with how it stands, what blocks it and who holds it, and
// a control that says how many are left.
test("the section stands above the outline in every view, opens expanded and lists 20 children before its control is pressed", async () => {
  answerReads();
  const issue = issueWith(23);
  for (const preset of PRESET_ORDER) {
    const where = `with ${PRESET_NAMES[preset]} on`;
    const view = drawFrame(issue, presetPath(KEY, preset));
    try {
      const slot = await waitFor(() => {
        const drawn = view.container.querySelector(SLOT_MARKS);
        expect(drawn, `no slot is drawn ${where}`).not.toBeNull();
        return drawn as HTMLElement;
      });
      const box = section(view);
      expect(box, `no children's section is drawn ${where}`).not.toBeNull();
      expect(
        (box as HTMLElement).compareDocumentPosition(slot) & Node.DOCUMENT_POSITION_FOLLOWING,
        `the section stands below the outline ${where}`
      ).toBeTruthy();
      expect(
        toggle(view).getAttribute("aria-expanded"),
        `the section opens collapsed ${where}`
      ).toBe("true");
      expect(box?.querySelector("ul")?.hidden, `the list is hidden ${where}`).toBe(false);
      expect(listed(view), `the section lists other than the first 20 children ${where}`).toEqual(
        childrenOf(23)
          .slice(0, CHILDREN_PAGE)
          .map((child) => child.key)
      );
      expect(more(view)?.textContent, `the control does not say 3 are left ${where}`).toBe(
        "3 more children"
      );
      const blocked = box?.querySelector('[data-child-entry="CORE-3"]');
      expect(blocked?.querySelector("[data-child-facts]")?.getAttribute("data-child-status")).toBe(
        "todo"
      );
      expect(
        blocked?.textContent,
        `a child's entry does not say what blocks it ${where}`
      ).toContain("CORE-4");
      expect(
        box?.querySelector('[data-child-entry="CORE-4"] [data-child-holder]'),
        `a child's entry does not say who holds it ${where}`
      ).not.toBeNull();
      expect(entryKey(view, "CORE-2")?.getAttribute("href")).toBe("/issues/CORE-2");
    } finally {
      view.unmount();
    }
  }
});

// R15: a press of the control lists the next 20 and takes no focus from that press: a pointer's
// leaves the focus where it was, and a press from the keyboard leaves it on the first child it
// listed.
test("the section's control lists the next 20, leaving the focus on the first one after a key and where it was after a pointer", async () => {
  answerReads();
  const user = userEvent.setup();
  const view = drawFrame(issueWith(45), presetPath(KEY, "conversation"));
  try {
    await waitFor(() => expect(listed(view)).toHaveLength(CHILDREN_PAGE));
    const control = more(view) as HTMLButtonElement;
    await act(async () => control.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(listed(view)).toHaveLength(2 * CHILDREN_PAGE));
    const first = entryKey(view, "CORE-22");
    expect(
      document.activeElement,
      "a key's press left the focus off the first child it listed"
    ).toBe(first);
    expect(more(view)?.textContent).toBe("5 more children");

    await user.click(more(view) as HTMLButtonElement);
    await waitFor(() => expect(listed(view)).toHaveLength(45));
    expect(more(view), "the control stays with no child left").toBeNull();
    expect(document.activeElement, "a pointer's press moved the focus").toBe(first);
  } finally {
    view.unmount();
  }
});

// R14: collapsed by the reader, the header keeps the bar and its two labelled counts, and the
// choice is kept as their own browser's preference, so the next issue they open draws the
// section the same way, until they open it again.
test("the reader's collapse keeps the bar and both counts, and the next issue opens collapsed", async () => {
  answerReads();
  const user = userEvent.setup();
  const first = drawFrame(issueWith(23), presetPath(KEY, "conversation"));
  try {
    await outlineDrawn(first);
    expect(listed(first)).toHaveLength(CHILDREN_PAGE);
    await user.click(toggle(first));
    expect(toggle(first).getAttribute("aria-expanded")).toBe("false");
    expect(section(first)?.querySelector("ul")?.hidden, "a collapsed list is drawn").toBe(true);
    const header = toggle(first).closest("div") as HTMLElement;
    const direct = header.querySelector('[data-testid="issue-progress-children"]');
    const whole = header.querySelector('[data-testid="issue-progress-subtree"]');
    expect(direct?.getAttribute("aria-label"), "the direct count is not kept").toBe(
      "8 of 23 children done"
    );
    expect(whole?.getAttribute("aria-label"), "the subtree's count is not kept").toBe(
      "10 of 27 of all issues under it done"
    );
    expect(direct?.textContent).toContain("Children");
    expect(whole?.textContent).toContain("All issues under it");
    expect(window.localStorage.getItem(KEPT)).toBe("collapsed");
  } finally {
    first.unmount();
  }

  const next = drawFrame(issueWith(3, "CORE-90"), presetPath("CORE-90", "conversation"));
  try {
    await waitFor(() =>
      expect(toggle(next).getAttribute("aria-expanded"), "the next issue forgot the choice").toBe(
        "false"
      )
    );
    expect(section(next)?.querySelector("ul")?.hidden).toBe(true);
    await user.click(toggle(next));
    expect(window.localStorage.getItem(KEPT)).toBe("expanded");
  } finally {
    next.unmount();
  }

  const again = drawFrame(issueWith(3), presetPath(KEY, "conversation"));
  try {
    await outlineDrawn(again);
    expect(toggle(again).getAttribute("aria-expanded")).toBe("true");
  } finally {
    again.unmount();
  }
});

// R14: an issue with no children draws no section, in any view.
test("an issue with no children draws no section", async () => {
  answerReads();
  for (const preset of PRESET_ORDER) {
    const view = drawFrame(issueDetails(), presetPath(KEY, preset));
    try {
      await waitFor(() => expect(view.container.querySelector(SLOT_MARKS)).not.toBeNull());
      expect(section(view), `a section is drawn with ${PRESET_NAMES[preset]} on`).toBeNull();
    } finally {
      view.unmount();
    }
  }
});

// R14: the children's own address is the page with no filter on and the focus on the section,
// and the outline lands on no row for it, the child's row among them, as its read arrives after
// the section has taken the focus, as a read from the network does.
test("/children puts the focus on the section, and the outline takes none of it", async () => {
  const issue = issueWith(2);
  answerReads();
  const tree = Promise.withResolvers<void>();
  const answered = treeWith(childrenOf(2));
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async () => {
    await tree.promise;
    return answered;
  });
  const view = drawFrame(issue, buildIssuePath({ key: KEY, kind: "children" }));
  try {
    await rowTakesFocus(view, CHILDREN_TOGGLE, "/children");
    await act(async () => tree.resolve());
    await waitFor(() =>
      expect(view.container.querySelector('[data-node-row][data-node-id="CORE-2"]')).not.toBeNull()
    );
    await frames();
    expect(
      document.activeElement,
      `the outline took the focus from the section, to ${focusedRow()}`
    ).toBe(toggle(view));
    expect(view.container.querySelector("[data-issue-body]"), "/children opened a view").not.toBe(
      null
    );
  } finally {
    view.unmount();
  }
});

// `t h` opens the children's own address and puts the focus on the section each time it is
// pressed, from a filter and from the outline, after the reader moved on, and from a zoom into a
// node of the outline, which holds no child.
test("t h puts the focus on the section from any view, and again after the reader moved on", async () => {
  const issue = issueWith(2);
  answerReads(childrenOf(2));
  const user = userEvent.setup();
  const view = drawFrame(issue, presetPath(KEY, "spec"));
  try {
    await waitFor(() => expect(view.container.querySelector(SLOT_MARKS)).not.toBeNull());
    await user.keyboard("th");
    await rowTakesFocus(view, CHILDREN_TOGGLE, "t h from Spec");
    expect(at.address).toBe(buildIssuePath({ key: KEY, kind: "children" }));

    const row = await waitFor(() => {
      const found = view.container.querySelector<HTMLElement>(
        '[data-node-row][data-node-id="head-a"]'
      );
      expect(found).not.toBeNull();
      return found as HTMLElement;
    });
    await act(async () => row.focus());
    await user.keyboard(".");
    await waitFor(() => expect(view.container.querySelector("[data-crumb]")).not.toBeNull());
    expect(focusedRow()).toBe("head-a");
    await user.keyboard("th");
    await rowTakesFocus(view, CHILDREN_TOGGLE, "t h from a zoom after the reader moved on");
  } finally {
    view.unmount();
  }
});

// `t h` is offered only where the page draws the section: on an issue with no children it opens
// no address and moves no focus.
test("t h on an issue with no children changes nothing", async () => {
  answerReads();
  const user = userEvent.setup();
  const view = drawFrame(issueDetails(), presetPath(KEY, "conversation"));
  try {
    await waitFor(() => expect(focusedRow()).toBe("head-a"));
    const before = at.address;
    await user.keyboard("th");
    await frames();
    expect(at.address).toBe(before);
    expect(focusedRow()).toBe("head-a");
  } finally {
    view.unmount();
  }
});

// R9: a child's kind tag on its row of the outline opens it in the children's section: the
// section opens, though the reader keeps it collapsed, lists the page of children that holds the
// child, and puts the focus on the child's entry. The opening keeps no choice of the reader's.
test("a child's tag on its row opens the section at its entry, on the page that holds it", async () => {
  const issue = issueWith(23);
  answerReads(childrenOf(23));
  window.localStorage.setItem(KEPT, "collapsed");
  const user = userEvent.setup();
  const view = drawFrame(issue, presetPath(KEY, "conversation"));
  try {
    await waitFor(() => expect(toggle(view).getAttribute("aria-expanded")).toBe("false"));
    const tag = await waitFor(() => {
      const found = view.container.querySelector<HTMLElement>(
        '[data-node-row][data-node-id="CORE-24"] [data-node-tag] [data-child-key]'
      );
      expect(found, "the outline drew no row with the child's tag").not.toBeNull();
      return found as HTMLElement;
    });
    expect(tag.tagName).toBe("BUTTON");
    expect(tag.textContent).toBe("CORE-24");

    await user.click(tag);
    expect(toggle(view).getAttribute("aria-expanded"), "the tag left the section closed").toBe(
      "true"
    );
    expect(section(view)?.querySelector("ul")?.hidden).toBe(false);
    expect(listed(view), "the tag did not show the page that holds the child").toHaveLength(23);
    expect(document.activeElement, "the tag left the focus off the child's entry").toBe(
      entryKey(view, "CORE-24")
    );
    expect(window.localStorage.getItem(KEPT), "the tag changed the reader's choice").toBe(
      "collapsed"
    );
  } finally {
    view.unmount();
  }
});
