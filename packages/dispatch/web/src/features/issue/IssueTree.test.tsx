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

test("the whole tree is one tab stop at rest, and the focused row lends its controls to it", async () => {
  setViewport(1440);
  const user = userEvent.setup();
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
  const [row] = rowsFor(view.container, "decisions#b-d1");
  if (row === undefined) throw new Error("the decision has no row");

  // At rest the tree offers one tab stop and nothing inside a row offers another, so Tab
  // enters the tree once and leaves it again (R9).
  const stops = [...view.container.querySelectorAll('[tabindex="0"]')];
  expect(stops.length).toBe(1);
  expect(stops[0]?.getAttribute("data-node-row")).toBe("");

  // Standing on the row, Tab reaches what the row holds: the chip's link, then Keep, then
  // Change, none of which any key presses (WCAG 2.1.1).
  await act(async () => row.focus());
  await waitFor(() => expect(view.getByRole("button", { name: "Keep" })).toBeTruthy());
  await user.tab();
  expect((document.activeElement as HTMLElement).textContent).toBe("The spec block");
  await user.tab();
  expect(document.activeElement).toBe(view.getByRole("button", { name: "Keep" }));
  await user.tab();
  expect(document.activeElement).toBe(view.getByRole("button", { name: "Change" }));
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
