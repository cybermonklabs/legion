import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Artifact, Decision, IssueDecisions } from "../../api/types";
import { MarginProvider } from "../margin/margin-context";
import { KeymapProvider } from "../shell/KeymapProvider";
import { useKeymapScope } from "../shell/keymap";
import { IssueTree, ZOOMED_TREE_QUERY } from "./IssueTree";
import type { TreeNode } from "./issue-tree";
import type { NodeType } from "./page-words";

/**
 * What a reader does to the tree with the keyboard alone (dispatch://INFRA-1196 R6, R9, R23).
 *
 * The tree is rendered here rather than through `IssuePage`, because what these rows are about
 * is where the focus goes and which control a key presses, and that needs a tree whose shape -
 * one node under two parents, two open questions in different branches - the page's own reads
 * would have to be fabricated backwards to produce.
 */

const KEY = "CORE-1";

const decisionsDocument: Artifact = {
  created_at: "2026-11-05T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-decisions",
  issue_key: KEY,
  kind: "doc",
  name: "Decisions",
  primary: false,
  project: "CORE",
  ref_key: "artifact:artifact-decisions",
  session_id: null,
  slug: "decisions",
  versions: [],
};

const decision: Decision = {
  asks: [],
  block_id: "d1",
  changed_at: null,
  changed_version: 1,
  heading: "Ship behind a flag",
  kept: null,
  kind: "agent",
  links: [],
  ref: `dispatch://${KEY}/decisions#b-d1`,
  replaced: false,
  text: "## Ship behind a flag\n\nWe ship it behind a flag.",
  text_sha256: "sha256:one",
  unsure: false,
  waiting_review: true,
};

function node(partial: Partial<TreeNode> & { id: string; type: NodeType }): TreeNode {
  return {
    alsoUnder: [],
    children: [],
    chips: [],
    facts: [],
    gaps: [],
    held: 0,
    question: false,
    replaced: false,
    text: partial.id,
    transcluded: false,
    waiting: false,
    ...partial,
  };
}

/** happy-dom answers every media query against its own 1024 px window, which is where the tree
 *  is read one node at a time (`ZOOMED_TREE_QUERY`). A test that wants the whole tree drawn
 *  asks for a wider one. */
function setViewport(width: number): void {
  window.matchMedia = ((query: string) =>
    ({
      addEventListener: () => {},
      addListener: () => {},
      dispatchEvent: () => true,
      matches: width <= Number(/max-width:\s*(\d+)/.exec(query)?.[1] ?? 0),
      media: query,
      onchange: null,
      removeEventListener: () => {},
      removeListener: () => {},
    }) as MediaQueryList) as typeof window.matchMedia;
}

const originalMatchMedia = window.matchMedia;
afterEach(() => {
  window.matchMedia = originalMatchMedia;
});

/** The page's own `issue` scope, which every one of the tree's keys is registered in. */
function IssueScope(): ReactNode {
  useKeymapScope("issue");
  return null;
}

function renderTree(
  root: TreeNode,
  { artifacts = [], decisions }: { artifacts?: Artifact[]; decisions?: IssueDecisions } = {}
) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  if (decisions !== undefined) client.setQueryData(["decisions", KEY], decisions);
  const reports: string[] = [];
  const edited: (string | undefined)[] = [];
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <MarginProvider>
            <IssueScope />
            <IssueTree
              artifacts={new Map(artifacts.map((artifact) => [artifact.slug, artifact]))}
              context={new Set()}
              emptyLine="Nothing here."
              filtering={false}
              focusReveal={false}
              isClosed={false}
              onEditing={(artifactId) => edited.push(artifactId)}
              onReport={(report) => reports.push(report)}
              reveal={undefined}
              root={root}
            />
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { edited, reports, ...view };
}

/** Every row drawn for `nodeId`, in the order the tree drew them: one per placement (R23). */
function rowsFor(container: HTMLElement, nodeId: string): HTMLElement[] {
  return [...container.querySelectorAll<HTMLElement>(`[data-node-row][data-node-id="${nodeId}"]`)];
}

function focusedRow(container: HTMLElement): string | undefined {
  const active = document.activeElement;
  return [...container.querySelectorAll<HTMLElement>("[data-node-row]")]
    .find((row) => row === active || row.contains(active))
    ?.getAttribute("data-node-id") as string | undefined;
}

test("the whole tree is one tab stop, and the focused row's keys work its controls", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const kept = spyOn(api, "keepDecision").mockResolvedValue({
    ...decision,
    waiting_review: false,
  } as never);
  const root = node({
    children: [
      node({
        chips: [
          {
            answered: true,
            gone: false,
            name: "The spec block",
            ref: `dispatch://${KEY}/spec#b-r1`,
          },
        ],
        id: "decisions#b-d1",
        reference: decision.ref,
        type: "decision",
        waiting: true,
      }),
    ],
    id: KEY,
    type: "issue",
  });
  const view = renderTree(root, {
    artifacts: [decisionsDocument],
    decisions: { decisions: [decision], document: null, open_questions: [] },
  });
  try {
    const [row] = rowsFor(view.container, "decisions#b-d1");
    if (row === undefined) throw new Error("the decision has no row");

    // The tree offers one tab stop and nothing inside a row offers another, standing on the
    // row included, so Tab enters the tree once and leaves it again (R9).
    await act(async () => row.focus());
    await waitFor(() => expect(view.getByRole("button", { name: "Keep" })).toBeTruthy());
    const stops = [...view.container.querySelectorAll('[tabindex="0"]')];
    expect(stops.length).toBe(1);
    expect(stops[0]?.getAttribute("data-node-row")).toBe("");

    // So each control of the row takes a key instead, and the keys reach what no Tab does
    // (WCAG 2.1.1): `f` steps to the chip's link, Escape gives the row back, Shift+K presses
    // Keep and Shift+C presses Change and leaves the caret in the form it opens.
    await user.keyboard("f");
    expect((document.activeElement as HTMLElement).textContent).toBe("The spec block");
    await user.keyboard("{Escape}");
    expect(document.activeElement).toBe(row);

    await user.keyboard("{Shift>}K{/Shift}");
    await waitFor(() => expect(kept).toHaveBeenCalled());
    expect(kept.mock.calls[0]?.[1]).toBe(decision.block_id);

    await user.keyboard("{Shift>}C{/Shift}");
    const field = await view.findByRole("textbox", { name: "Your change" });
    await waitFor(() => expect(document.activeElement).toBe(field));
  } finally {
    kept.mockRestore();
  }
});

test("Enter sends the change the reader typed, instead of putting a caret in the row", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const sent = spyOn(api, "createComment").mockResolvedValue({ id: "comment-1" } as never);
  const root = node({
    children: [
      node({
        id: "decisions#b-d1",
        reference: decision.ref,
        type: "decision",
        waiting: true,
      }),
    ],
    id: KEY,
    type: "issue",
  });
  const view = renderTree(root, {
    artifacts: [decisionsDocument],
    decisions: { decisions: [decision], document: null, open_questions: [] },
  });
  try {
    const [row] = rowsFor(view.container, "decisions#b-d1");
    await act(async () => row?.focus());
    await user.click(await view.findByRole("button", { name: "Change" }));
    await user.click(view.getByRole("textbox", { name: "Your change" }));
    await user.keyboard("Say more about the flag.");

    // Enter on the form's own control sends it: the row's Enter fires from the row itself and
    // takes nothing from a control the reader reached inside it.
    await act(async () => view.getByRole("button", { name: "Send change" }).focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(sent).toHaveBeenCalled());
    expect(sent.mock.calls[0]?.[0]).toBe(KEY);
    // Nothing went into an editor: the row's Enter never fired.
    expect(view.edited.filter((artifactId) => artifactId !== undefined)).toEqual([]);
  } finally {
    sent.mockRestore();
  }
});

/** The one decision's tree the two rows below press its controls in. */
function decisionTree(): TreeNode {
  return node({
    children: [
      node({ id: "decisions#b-d1", reference: decision.ref, type: "decision", waiting: true }),
    ],
    id: KEY,
    type: "issue",
  });
}

// A control that disables itself while its write is out drops the focus to the document body
// if it holds it: a keyboard reader loses their place, and the tree closes the body the focus
// held, taking Change with it (measured in Chromium: a `focusout` from Keep with no related
// target 45 ms after its write went out, then the kept decision's body removed). So Keep moves
// the focus onto its decision's own row first, and the row's body stays open throughout.
test("Keep keeps the focus on its decision's row while its write is out, and after", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const answer = Promise.withResolvers<never>();
  const kept = spyOn(api, "keepDecision").mockReturnValue(answer.promise);
  const view = renderTree(decisionTree(), {
    artifacts: [decisionsDocument],
    decisions: { decisions: [decision], document: null, open_questions: [] },
  });
  try {
    const [row] = rowsFor(view.container, "decisions#b-d1");
    if (row === undefined) throw new Error("the decision has no row");
    await act(async () => row.focus());
    const keep = await view.findByRole("button", { name: "Keep" });
    await user.click(keep);
    await waitFor(() => expect(kept).toHaveBeenCalled());
    await waitFor(() => expect(keep).toHaveProperty("disabled", true));
    expect(document.activeElement, "the focus stayed on Keep, which its write disables").toBe(row);
    expect(row.querySelector("[data-node-body]"), "the decision's body closed").not.toBeNull();

    await act(async () => answer.resolve({ ...decision, waiting_review: false } as never));
    await waitFor(() => expect(view.queryByRole("button", { name: "Keep" })).toBeNull());
    expect(document.activeElement).toBe(row);
    expect(view.getByRole("button", { name: "Change" })).toBeTruthy();
  } finally {
    kept.mockRestore();
  }
});

// The same for Change's own Send, which is disabled while the comment is out and goes with the
// form once it lands.
test("Send change keeps the focus on its decision's row while the comment is out, and after", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const answer = Promise.withResolvers<never>();
  const sent = spyOn(api, "createComment").mockReturnValue(answer.promise);
  const view = renderTree(decisionTree(), {
    artifacts: [decisionsDocument],
    decisions: { decisions: [decision], document: null, open_questions: [] },
  });
  try {
    const [row] = rowsFor(view.container, "decisions#b-d1");
    if (row === undefined) throw new Error("the decision has no row");
    await act(async () => row.focus());
    await user.click(await view.findByRole("button", { name: "Change" }));
    await user.keyboard("Say more about the flag.");
    const send = view.getByRole("button", { name: "Send change" });
    await user.click(send);
    await waitFor(() => expect(sent).toHaveBeenCalled());
    await waitFor(() => expect(send).toHaveProperty("disabled", true));
    expect(
      document.activeElement,
      "the focus stayed on Send change, which its write disables"
    ).toBe(row);
    expect(row.querySelector("[data-node-body]"), "the decision's body closed").not.toBeNull();

    await act(async () => answer.resolve({ id: "comment-1" } as never));
    await waitFor(() => expect(view.queryByRole("textbox", { name: "Your change" })).toBeNull());
    expect(document.activeElement).toBe(row);
    expect(view.getByRole("button", { name: "Change" })).toBeTruthy();
  } finally {
    sent.mockRestore();
  }
});

test("Escape leaves the caret for the placement it was taken from, not the node's first row", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const spec: Artifact = { ...decisionsDocument, id: "artifact-spec", name: "Spec", slug: "spec" };
  const shared = { id: "spec#b-c1", text: "One check, under two requirements.", type: "check" };
  const root = node({
    children: [
      node({ children: [node(shared as never)], id: "spec#b-r1", type: "requirement" }),
      node({ children: [node(shared as never)], id: "spec#b-r2", type: "requirement" }),
    ],
    id: KEY,
    type: "issue",
  });
  const view = renderTree(root, { artifacts: [spec] });
  const placements = rowsFor(view.container, "spec#b-c1");
  expect(placements.length).toBe(2);
  const second = placements[1];
  if (second === undefined) throw new Error("the check has one placement only");

  await act(async () => second.focus());
  await user.keyboard("{Enter}");
  await waitFor(() => expect(view.edited.at(-1)).toBe("artifact-spec"));
  await user.keyboard("{Escape}");
  await waitFor(() => expect(view.edited.at(-1)).toBeUndefined());
  expect(document.activeElement).toBe(second);
});

test("q reaches every open question while the tree is read one node at a time", async () => {
  setViewport(1024);
  expect(window.matchMedia(ZOOMED_TREE_QUERY).matches).toBe(true);
  const user = userEvent.setup();
  const root = node({
    children: [
      node({
        children: [node({ id: "ask-1", question: true, text: "Which flag?", type: "ask" })],
        id: "spec",
        type: "document",
      }),
      node({
        children: [node({ id: "ask-2", question: true, text: "Who ships it?", type: "ask" })],
        id: "CORE-2",
        type: "child_issue",
      }),
    ],
    id: KEY,
    type: "issue",
  });
  const view = renderTree(root);

  await user.keyboard("q");
  await waitFor(() => expect(focusedRow(view.container)).toBe("ask-1"));
  await user.keyboard("q");
  await waitFor(() => expect(focusedRow(view.container)).toBe("ask-2"));
  expect(view.reports.at(-1)).toBe("");
});

// R6: `.` zooms into the focused node, and the key then takes the reader to that node so its
// row keeps the focus (`useTreeKeymap.ts`'s `tree-zoom-in`). Where the tree is read one node
// at a time, taking a reader to a node zooms to that node's parent, which undid the zoom the
// key had just made: `.` drew the rows it started from, and on the zoomed node's own row it
// walked back out one level instead.
test(". zooms into the focused node while the tree is read one node at a time", async () => {
  setViewport(1024);
  const user = userEvent.setup();
  const root = node({
    children: [
      node({
        children: [node({ id: "spec#b-r1", type: "requirement" })],
        id: "spec",
        type: "document",
      }),
    ],
    id: KEY,
    type: "issue",
  });
  const view = renderTree(root);
  const [row] = rowsFor(view.container, "spec");
  if (row === undefined) throw new Error("the document has no row");

  await act(async () => row.focus());
  await user.keyboard(".");
  // The tree is on the document now: its own row and what it holds, with the issue left
  // behind in the breadcrumb above the rows.
  await waitFor(() => expect(rowsFor(view.container, "spec#b-r1").length).toBe(1));
  expect(rowsFor(view.container, KEY).length).toBe(0);
  expect(view.getByRole("button", { name: KEY })).toBeTruthy();

  // On the zoomed node's own row - which is where the key left the focus - `.` holds the zoom
  // where it is rather than taking the reader back out of it.
  await user.keyboard(".");
  expect(rowsFor(view.container, "spec#b-r1").length).toBe(1);
  expect(rowsFor(view.container, KEY).length).toBe(0);
});

// R9: a row's key is the path from the issue down to it, and it holds the pin, the open body
// and the tree's one tab stop. The whole tree drawn from a zoomed node keyed its rows from
// that node, so a zoom re-keyed every row below it: a pin made before the zoom closed as the
// reader zoomed in, and one made while zoomed was lost on the way out.
test("a row keeps its key, and its pin, through a zoom in and back out", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const root = node({
    children: [
      node({
        children: [node({ id: "verification#b-c1", type: "check" })],
        id: "spec#b-r1",
        type: "requirement",
      }),
    ],
    id: KEY,
    type: "issue",
  });
  const view = renderTree(root);
  const [check] = rowsFor(view.container, "verification#b-c1");
  const [requirement] = rowsFor(view.container, "spec#b-r1");
  if (check === undefined || requirement === undefined) throw new Error("the tree drew no rows");
  const keyOfCheck = check.getAttribute("data-row-key");

  await act(async () => check.focus());
  await user.keyboard("o");
  await waitFor(() => expect(check.querySelector("[data-pinned]")).toBeTruthy());

  // Zoomed into the requirement, the check is the same row under the same key, so the body
  // the reader pinned open is still open.
  await act(async () => requirement.focus());
  await user.keyboard(".");
  await waitFor(() => expect(rowsFor(view.container, KEY).length).toBe(0));
  const [zoomedCheck] = rowsFor(view.container, "verification#b-c1");
  expect(zoomedCheck?.getAttribute("data-row-key")).toBe(keyOfCheck);
  expect(zoomedCheck?.querySelector("[data-pinned]")).toBeTruthy();

  // A pin made while zoomed is on the same row once the reader is back out of the zoom.
  await user.keyboard("o");
  await waitFor(() =>
    expect(rowsFor(view.container, "spec#b-r1")[0]?.querySelector("[data-pinned]")).toBeTruthy()
  );
  await user.keyboard(",");
  await waitFor(() => expect(rowsFor(view.container, KEY).length).toBe(1));
  expect(rowsFor(view.container, "spec#b-r1")[0]?.querySelector("[data-pinned]")).toBeTruthy();
  expect(
    rowsFor(view.container, "verification#b-c1")[0]?.querySelector("[data-pinned]")
  ).toBeTruthy();
});
