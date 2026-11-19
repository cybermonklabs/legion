import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type {
  Artifact,
  ArtifactBlock,
  Decision,
  IssueDecisions,
  IssueDetails,
} from "../../api/types";
import { MarginProvider } from "../margin/margin-context";
import { buildDispatchReference } from "../refs/routes";
import { KeymapProvider } from "../shell/KeymapProvider";
import { useKeymapScope } from "../shell/keymap";
import { IssueThreeThings, specSummary } from "./IssueThreeThings";
import { IssueTree, type OutlineRequest, ZOOMED_TREE_QUERY } from "./IssueTree";
import { buildIssueTree, type TreeNode } from "./issue-tree";
import { KEY_REPORTS } from "./page-structure";
import type { NodeType } from "./page-words";
import { blockNodeId } from "./tree-drafts";

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

/** happy-dom answers every media query against its own 1024 px window, where the tree draws
 *  three levels (`ZOOMED_TREE_QUERY` is below 1024 px). A test sets the width it reads: a
 *  `max-width` in px, a `min-width` in rem, and `not all and` either. */
function setViewport(width: number): void {
  window.matchMedia = ((query: string) => {
    const max = /max-width:\s*([\d.]+)px/.exec(query)?.[1];
    const min = /min-width:\s*([\d.]+)rem/.exec(query)?.[1];
    const matched =
      max !== undefined ? width <= Number(max) : min !== undefined && width >= Number(min) * 16;
    return {
      addEventListener: () => {},
      addListener: () => {},
      dispatchEvent: () => true,
      matches: query.startsWith("not all and ") ? !matched : matched,
      media: query,
      onchange: null,
      removeEventListener: () => {},
      removeListener: () => {},
    } as MediaQueryList;
  }) as typeof window.matchMedia;
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
  {
    artifacts = [],
    decisions,
    request,
  }: { artifacts?: Artifact[]; decisions?: IssueDecisions; request?: OutlineRequest } = {}
) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  if (decisions !== undefined) client.setQueryData(["decisions", KEY], decisions);
  const edited: (string | undefined)[] = [];
  const draw = (tree: TreeNode) => (
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
              holdsEscape={false}
              isClosed={false}
              onEditing={(artifactId) => edited.push(artifactId)}
              {...(request === undefined ? {} : { request })}
              reveal={undefined}
              root={tree}
              rootRow
            />
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(draw(root));
  // The tree again as a read that lands draws it: the same page, a new model.
  const redraw = (tree: TreeNode) => view.rerender(draw(tree));
  return { edited, redraw, ...view };
}

/** What a key reported at the row it was pressed on, or undefined where no row draws one. */
function rowReport(container: HTMLElement): string | undefined {
  return container.querySelector("[data-row-report]")?.textContent ?? undefined;
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
  setViewport(1000);
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
  expect(rowReport(view.container)).toBeUndefined();
});

// R6: `.` zooms into the focused node, and the key then takes the reader to that node so its
// row keeps the focus (`useTreeKeymap.ts`'s `tree-zoom-in`). Where the tree is read one node
// at a time, taking a reader to a node zooms to that node's parent, which undid the zoom the
// key had just made: `.` drew the rows it started from, and on the zoomed node's own row it
// walked back out one level instead.
test(". zooms into the focused node while the tree is read one node at a time", async () => {
  setViewport(1000);
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

// R16: below 1024 px the outline draws one level under the node it is read from, and a deeper
// branch is one line saying how many nodes it holds, which opens them in place. Unfolding a
// branch on a phone draws that line under it, beside the rows above, and pressing the line
// draws the nodes it holds there - it does not carry the reader off to another root.
test("on a phone a branch's counted line opens its nodes in place", async () => {
  setViewport(390);
  const user = userEvent.setup();
  const turn = (id: string, children: TreeNode[] = []) =>
    node({
      children,
      held: children.reduce((total, child) => total + child.held + 1, 0),
      id,
      reference: buildDispatchReference({ id, key: KEY, kind: "comment" }),
      text: `Turn ${id}.`,
      type: "comment",
    });
  const root = node({
    children: [turn("head", [turn("first", [turn("second", [turn("third")])])])],
    id: KEY,
    type: "issue",
  });
  const view = renderTree(root);
  const head = rowsFor(view.container, "head")[0]?.querySelector<HTMLButtonElement>(
    'button[aria-label="Unfold"]'
  );
  if (head === null || head === undefined) throw new Error("the head has no Unfold");

  await user.click(head);
  // The head is open, and what it holds is one line counting its three nodes, under it.
  const line = await waitFor(() => {
    const found = view.container.querySelector<HTMLElement>("[data-branch-more]");
    if (found === null) throw new Error("no branch is one line counting its nodes");
    return found;
  });
  expect(line.textContent).toContain("3");
  expect(rowsFor(view.container, "head")).toHaveLength(1);
  expect(rowsFor(view.container, "first")).toHaveLength(0);

  const open = line.querySelector("button");
  if (open === null) throw new Error("the counted line opens nothing");
  await user.click(open);
  // Pressing the line draws what it holds in its place, under the head the reader was on: the
  // outline is still read from the issue, with no crumb above it.
  await waitFor(() => expect(rowsFor(view.container, "first")).toHaveLength(1));
  expect(rowsFor(view.container, "head")).toHaveLength(1);
  expect(view.container.querySelector("[data-crumb]")).toBeNull();
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
  await waitFor(() => expect(view.container.querySelector(`[data-crumb="${KEY}"]`)).not.toBeNull());
  const [zoomedCheck] = rowsFor(view.container, "verification#b-c1");
  expect(zoomedCheck?.getAttribute("data-row-key")).toBe(keyOfCheck);
  expect(zoomedCheck?.querySelector("[data-pinned]")).toBeTruthy();

  // A pin made while zoomed is on the same row once the reader is back out of the zoom.
  await user.keyboard("o");
  await waitFor(() =>
    expect(rowsFor(view.container, "spec#b-r1")[0]?.querySelector("[data-pinned]")).toBeTruthy()
  );
  await user.keyboard(",");
  await waitFor(() => expect(view.container.querySelector("[data-crumb]")).toBeNull());
  expect(rowsFor(view.container, "spec#b-r1")[0]?.querySelector("[data-pinned]")).toBeTruthy();
  expect(
    rowsFor(view.container, "verification#b-c1")[0]?.querySelector("[data-pinned]")
  ).toBeTruthy();
});

/** The issue the drawn-gap row reads: a spec with a summary and one requirement no check
 *  serves, and a decisions document with one decision, as the page's reads answer them. */
function fullyReadIssue(): TreeNode {
  const spec: Artifact = {
    ...decisionsDocument,
    id: "artifact-spec",
    name: "spec.md",
    primary: true,
    slug: "spec",
  };
  const details = {
    advances: [],
    advances_revision: 0,
    artifacts: [spec, decisionsDocument],
    assignee: null,
    blocked_by: [],
    children: [],
    claim: null,
    closed_at: null,
    components: { ids: [], inherited_from: null, mode: "inherit", reason: null, unknown: [] },
    created_at: "2026-11-05T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    decisions_artifact_id: decisionsDocument.id,
    design_gates: [],
    external_links: [],
    holder_state: null,
    key: KEY,
    labels: [],
    last_seen: null,
    last_seq: 1,
    number: 1,
    open_asks: [],
    parent: null,
    plan_artifact_id: "",
    primary_artifact_id: spec.id,
    priority: null,
    progress: { children: null, tasks: null },
    project: "CORE",
    rank: "U",
    referenced_by_count: 0,
    repo: null,
    resolved_repo: null,
    route: null,
    route_holder: null,
    route_status: null,
    route_status_reason: null,
    status: "todo",
    takeable_at: null,
    title: "The issue page draws one ordered body",
    updated_at: "2026-11-05T00:00:00Z",
    verification_artifact_id: "",
  } satisfies IssueDetails;
  const block = (id: string, type: string): ArtifactBlock => ({
    attributes: [],
    from: 0,
    id,
    references: { asks: 0, comments: 0 },
    to: 1,
    token: id,
    type,
  });
  return buildIssueTree({
    coverage: {
      gaps: ["r1 has no check"],
      has_plan: false,
      issue: KEY,
      requirements: [
        {
          block_id: "r1",
          checks: 0,
          gaps: ["r1 has no check"],
          plan_tasks: 0,
          ref: `dispatch://${KEY}/spec#b-r1`,
          text: "The page draws the whole issue in one reading.",
        },
      ],
      spec_artifact_id: spec.id,
    },
    decisions: {
      decisions: [
        { ...decision, ref: `dispatch://${KEY}/artifact/decisions#b-d1`, waiting_review: false },
      ],
      document: decisionsDocument.id,
      open_questions: [],
    },
    documents: [
      { artifactId: spec.id, blocks: [block("summary", "callout"), block("r1", "requirement")] },
    ],
    issue: details,
    targets: [
      {
        gone: false,
        kind: "block",
        name: ":::callout{#summary}\nWhat this is. Why it matters. What is different when done.\n:::",
        ref: buildDispatchReference({
          block: { id: "summary" },
          key: KEY,
          kind: "artifact",
          slug: "spec",
        }),
      },
    ],
  });
}

// R10, R22 and the person's ruling on helper text: a gap is drawn only where it tells a node
// apart from its siblings and names work somebody owes. Under today's model every requirement
// and every decision lacks an origin, and no issue records who wrote or confirmed its summary,
// so on a fully read issue the requirement, the decision and the head draw none of those three -
// while the requirement still draws the gap that does tell it apart: no check serves it.
test("a requirement, a decision and the head of a fully read issue draw no gap their whole kind shares", async () => {
  setViewport(1440);
  const root = fullyReadIssue();
  const view = renderTree(root, { artifacts: [decisionsDocument] });
  const head = render(
    <MemoryRouter>
      <IssueThreeThings look="pane" reading={false} summary={specSummary(root, "spec")} />
    </MemoryRouter>
  );
  try {
    const requirement = blockNodeId("spec", "r1");
    const settled = blockNodeId("decisions", "d1");
    await waitFor(() => expect(rowsFor(view.container, requirement).length).toBe(1));
    expect(rowsFor(view.container, settled).length).toBe(1);
    const kinds = (element: Element | undefined) =>
      [...(element?.querySelectorAll("[data-gap]") ?? [])].map((gap) =>
        gap.getAttribute("data-gap-kind")
      );
    expect(kinds(rowsFor(view.container, requirement)[0])).toEqual(["no_check"]);
    expect(kinds(rowsFor(view.container, settled)[0])).toEqual([]);
    expect(head.container.querySelector("li [data-author-text]")?.textContent).toBe(
      "What this is."
    );
    expect(kinds(head.container)).toEqual([]);
    for (const words of ["No source given", "Not confirmed in Dispatch", "No author in Dispatch"]) {
      expect(view.container.textContent).not.toContain(words);
      expect(head.container.textContent).not.toContain(words);
    }
  } finally {
    head.unmount();
    view.unmount();
  }
});

// The architect's ruling 5: the header is the issue, so the outline draws no row that repeats its
// key and title, and its first row is the first node under the issue. R7: the page opens with
// the focus on that row where no open question waits on the reader.
test("the outline draws no row of the issue, and opens with the focus on its first row", async () => {
  setViewport(1440);
  const root = node({
    children: [
      node({ id: "comment-1", text: "The first words.", type: "comment" }),
      node({ id: "ask-1", question: true, text: "Which flag?", type: "ask" }),
    ],
    id: KEY,
    text: `${KEY} The issue's own title`,
    type: "issue",
  });
  const view = renderTree(root);
  expect(rowsFor(view.container, KEY)).toHaveLength(0);
  const rows = [...view.container.querySelectorAll("[data-node-row]")];
  expect(rows.map((row) => row.getAttribute("data-node-id"))).toEqual(["comment-1", "ask-1"]);
  expect(rows.map((row) => row.getAttribute("aria-level"))).toEqual(["1", "1"]);
  // The open question waits on somebody else, so the first row is where the page opens.
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
});

// R7: the page opens with the focus on the first open question that waits on the reader, in the
// outline's own order, its path unfolded.
test("the page opens with the focus on the first open question that waits on the reader", async () => {
  setViewport(1440);
  const root = node({
    children: [
      node({ id: "comment-1", text: "The first words.", type: "comment" }),
      node({
        children: [
          node({ id: "ask-theirs", question: true, text: "Not yours?", type: "ask" }),
          node({ id: "ask-mine", question: true, text: "Yours?", type: "ask", waiting: true }),
        ],
        held: 2,
        id: "comment-2",
        text: "A thread.",
        type: "comment",
      }),
      node({ id: "ask-later", question: true, text: "Also yours?", type: "ask", waiting: true }),
    ],
    id: KEY,
    type: "issue",
  });
  const view = renderTree(root);
  await waitFor(() => expect(focusedRow(view.container)).toBe("ask-mine"));
});

/** A stand-in for the browser's `ResizeObserver`: `grow(target)` answers every observer watching
 *  `target`, as a change of that element's size would, and no other. */
function installResizeObserver(): { grow: (target: Element) => void; restore: () => void } {
  const original = globalThis.ResizeObserver;
  const live = new Set<ObserverStub>();

  class ObserverStub {
    readonly callback: ResizeObserverCallback;
    readonly targets = new Set<Element>();

    constructor(callback: ResizeObserverCallback) {
      this.callback = callback;
      live.add(this);
    }

    disconnect(): void {
      live.delete(this);
    }

    observe(target: Element): void {
      this.targets.add(target);
    }

    unobserve(target: Element): void {
      this.targets.delete(target);
    }
  }

  globalThis.ResizeObserver = ObserverStub as unknown as typeof ResizeObserver;
  return {
    grow: (target) => {
      for (const observer of [...live]) {
        if (observer.targets.has(target)) {
          observer.callback([], observer as unknown as ResizeObserver);
        }
      }
    },
    restore: () => {
      globalThis.ResizeObserver = original;
    },
  };
}

// R7: the question the page opens on is in the viewport, and stays there while the page lands
// around it. The outline's other reads arrive after the question's - forty comments above it on
// the browser suite's seed - and push its row below the screen without changing the row's own
// size, so the row's own placing never sees it move: the page holds it until the reader acts.
test("the open question the page opens on is held on screen as the page grows, until the reader acts", async () => {
  setViewport(1440);
  const observers = installResizeObserver();
  const scrolled = spyOn(window, "scrollBy").mockImplementation(() => {});
  try {
    const root = node({
      children: [
        node({ id: "comment-1", text: "The first words.", type: "comment" }),
        node({ id: "ask-mine", question: true, text: "Yours?", type: "ask", waiting: true }),
      ],
      id: KEY,
      type: "issue",
    });
    const view = renderTree(root);
    await waitFor(() => expect(focusedRow(view.container)).toBe("ask-mine"));
    const [question] = rowsFor(view.container, "ask-mine");
    if (question === undefined) throw new Error("the tree draws no row for ask-mine");
    // What landed above it put its 28 px row 400 px below the screen's foot.
    const pushedDown = new DOMRect(0, window.innerHeight + 400, 600, 28);
    spyOn(question, "getBoundingClientRect").mockReturnValue(pushedDown);
    scrolled.mockClear();
    observers.grow(document.body);
    // Its foot is brought 8 px clear of the screen's (`identity-block.ts`'s `bringOnScreen`).
    expect(scrolled).toHaveBeenCalledWith({ top: 436 });
    // A wheel is the reader taking over: the page grows on and moves them no more.
    window.dispatchEvent(new Event("wheel"));
    scrolled.mockClear();
    observers.grow(document.body);
    expect(scrolled).not.toHaveBeenCalled();
  } finally {
    scrolled.mockRestore();
    observers.restore();
  }
});

// R7, as the browser suite measured it: the page put the focus on the outline's first row, and
// that row was drawn again as the spec's blocks landed above it. The browser leaves the focus on
// the body then, and the page opened with the focus on no row at all. Until the reader acts, a
// focus that is nowhere is still the page's to place: on the outline's first row as it now is.
test("a focus a redraw drops while the page opens goes back to the outline's first row", async () => {
  setViewport(1440);
  const comments = [
    node({ id: "comment-1", text: "The first words.", type: "comment" }),
    node({ id: "comment-2", text: "The second words.", type: "comment" }),
  ];
  const view = renderTree(node({ children: comments, id: KEY, type: "issue" }));
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
  (document.activeElement as HTMLElement | null)?.blur();
  view.redraw(
    node({
      children: [
        node({ id: "comment-0", text: "Words above them.", type: "comment" }),
        ...comments,
      ],
      id: KEY,
      type: "issue",
    })
  );
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-0"));
});

// The same for the question the page opened on, which is where the page stays: a redraw that
// drops the focus gives it back to the question, and an answer arriving for it moves nothing.
test("the question the page opened on takes back a focus a redraw drops, and keeps it", async () => {
  setViewport(1440);
  const tree = (waiting: boolean) =>
    node({
      children: [
        node({ id: "comment-1", text: "The first words.", type: "comment" }),
        node({ id: "ask-mine", question: true, text: "Yours?", type: "ask", waiting }),
      ],
      id: KEY,
      type: "issue",
    });
  const view = renderTree(tree(true));
  await waitFor(() => expect(focusedRow(view.container)).toBe("ask-mine"));
  (document.activeElement as HTMLElement | null)?.blur();
  view.redraw(tree(true));
  await waitFor(() => expect(focusedRow(view.container)).toBe("ask-mine"));
  view.redraw(tree(false));
  await waitFor(() => expect(view.container.querySelectorAll("[data-node-row]")).toHaveLength(2));
  expect(focusedRow(view.container)).toBe("ask-mine");
});

// The tester's F16: what a key reports when it has nowhere to go is drawn at the row it was
// pressed on, and the reader's next move lets it go.
test("a key with nowhere to go says so at its row, until the next move", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const root = node({
    children: [
      node({ id: "comment-1", text: "The first words.", type: "comment" }),
      node({ id: "comment-2", text: "The second words.", type: "comment" }),
    ],
    id: KEY,
    type: "issue",
  });
  const view = renderTree(root);
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
  await user.keyboard("w");
  await waitFor(() => expect(rowReport(view.container)).toBe(KEY_REPORTS.no_decision));
  const [first] = rowsFor(view.container, "comment-1");
  expect(first?.querySelector("[data-row-report]")).not.toBeNull();
  await user.keyboard("j");
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-2"));
  expect(rowReport(view.container)).toBeUndefined();
});

// The reviewer's finding on the Contents pane's asks: an ask is acted on once, whatever draws
// the tree again in between - a document opened in its place and closed mounts a new tree with
// the page's last ask still in hand, and that ask must not move the reader a second time.
test("an ask of the Contents pane is acted on once, across the tree being drawn again", async () => {
  setViewport(1440);
  const root = node({
    children: [
      node({ id: "comment-1", text: "The first words.", type: "comment" }),
      node({ id: "comment-2", text: "The second words.", type: "comment" }),
    ],
    id: KEY,
    type: "issue",
  });
  const request: OutlineRequest = { focusBranch: false, id: "comment-2", seq: 1 };
  const first = renderTree(root, { request });
  await waitFor(() => expect(focusedRow(first.container)).toBe("comment-2"));
  first.unmount();
  const again = renderTree(root, { request });
  await waitFor(() => expect(focusedRow(again.container)).toBe("comment-1"));
  expect(focusedRow(again.container)).not.toBe("comment-2");
});

// The architect's ruling 1: a crumb draws one line for a node, so it is R1's first line, cut at a
// word, never cut again mid-word by its box; the issue's crumb is its key.
test("a crumb is its node's first line cut at a word, and the issue's is its key", async () => {
  setViewport(1000);
  const user = userEvent.setup();
  const long =
    "A first sentence that runs on well past the hundred characters a first line may hold so it is cut at a word boundary";
  const root = node({
    children: [
      node({
        children: [node({ id: "comment-2", text: "Under it.", type: "comment" })],
        held: 1,
        id: "comment-1",
        text: long,
        type: "comment",
      }),
    ],
    id: KEY,
    text: `${KEY} The issue's own title`,
    type: "issue",
  });
  const view = renderTree(root);
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
  await user.keyboard(".");
  await waitFor(() => expect(rowsFor(view.container, "comment-2")).toHaveLength(1));
  await user.keyboard("j");
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-2"));
  await user.keyboard(".");
  const crumbs = await waitFor(() => {
    const found = [...view.container.querySelectorAll<HTMLElement>("[data-crumb]")];
    if (found.length < 2) throw new Error("the breadcrumb holds no crumb of the comment");
    return found;
  });
  expect(crumbs[0]?.textContent).toBe(KEY);
  const crumb = crumbs[1]?.textContent ?? "";
  expect(crumb.endsWith("…")).toBe(true);
  expect(long.startsWith(crumb.slice(0, -1))).toBe(true);
  expect(long.charAt(crumb.length - 1)).toBe(" ");
  expect(crumbs[1]?.className).not.toContain("truncate");
});
