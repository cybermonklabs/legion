import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { act, type RenderResult, render, screen, waitFor } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { http } from "msw";
import { type ReactNode, useLayoutEffect, useMemo, useState } from "react";
import { Link, MemoryRouter, useLocation, useNavigate, useNavigationType } from "react-router-dom";

import { type FakeDocumentRuntime, fakeDocumentRuntime } from "../../__tests__/document-runtime";
import { nextFrame } from "../../__tests__/frames";
import { graphNode } from "../../__tests__/graph-fixture";
import { answerRequests } from "../../__tests__/network";
import {
  artifactRecordHandler,
  decisionsAndReadMarkHandlers,
} from "../../__tests__/proof-document";
import { api } from "../../api/client";
import type {
  ArtifactBlock,
  GraphReferences,
  IssueDetails,
  IssueReferences,
} from "../../api/types";
import { DocumentRuntime, type DocumentRuntimeValue } from "../doc/runtime";
import { Margin } from "../margin/Margin";
import { DESKTOP_WIDTH } from "../margin/margin-fixture";
import { buildIssuePath, parseIssuePath } from "../refs/routes";
import { DOCUMENT_OPEN_ATTRIBUTE, DOCUMENTS_LIST_ATTRIBUTE } from "./DocumentsList";
import {
  artifact,
  issueDetails,
  KEY,
  SPEC_SLUG,
  specRef,
} from "./decisions-filter/decisions-fixtures";
import { presetPath } from "./issue-address";
import {
  DETAILS_ATTRIBUTE,
  type MarginDrawer,
  type MarginDrawerPage,
  MarginSlotProvider,
} from "./margin-slot";
import { TWO_THREADS } from "./outline-fixtures";
import { CONTROL_LABELS } from "./page-words";
import { useFollowing } from "./slots/following";
import { ProvidersSlot } from "./slots/providers";
import { StageSlot } from "./slots/stage";
import { AppProviders, focusedRow, stubReads } from "./slots/stage-fixtures";
import { ViewRowSlot } from "./slots/view-row";
import { blockNodeId } from "./statement-rules";
import { nodeRowSelectorOf } from "./tree-focus";
import { check, VERIFICATION_SLUG } from "./verification-filter/verification-fixtures";

/**
 * The page's Escape order (part N's R1, the plan's ST-2) and the outline's scroll place across a
 * round trip (ST-5), one case per row of each table. Each case draws the page's own view row
 * over the page's own stage - the outline and every view as the page draws them - at the address
 * the router holds, and presses keys and controls as the reader does.
 */

/** The plan, the Verification document and a document of no role, beside the spec and the
 *  decisions document. */
const PLAN_DOCUMENT = artifact("artifact-plan", "plan-md");
const VERIFICATION_DOCUMENT = artifact("artifact-verification", VERIFICATION_SLUG);
const NOTES = artifact("artifact-notes", "notes-md");

const ISSUE: IssueDetails = (() => {
  const issue = issueDetails();
  return {
    ...issue,
    artifacts: [...(issue.artifacts ?? []), PLAN_DOCUMENT, VERIFICATION_DOCUMENT, NOTES],
    plan_artifact_id: PLAN_DOCUMENT.id,
    verification_artifact_id: VERIFICATION_DOCUMENT.id,
  };
})();

/** The spec's one statement, and the plan's task and the Verification document's check that
 *  each serve it. */
const STATEMENT = "r1";
const PLAN_TASK = "t1";
const CHECK = "c1";
const STATEMENT_NODE = blockNodeId(SPEC_SLUG, STATEMENT);
const TASK_NODE = blockNodeId(PLAN_DOCUMENT.slug, PLAN_TASK);
const CHECK_NODE = blockNodeId(VERIFICATION_SLUG, CHECK);

/** Words the statement holds, which a words filter keeps it under. */
const WORDS = "plan";

/** The outline's own address, every view off, which Conversation, `t c` and Escape open. */
const CONVERSATION = presetPath(KEY, "conversation");

/** Documents' list's own address (part N's R2). */
const DOCUMENTS = buildIssuePath({ key: KEY, kind: "issue-documents" });

/** The notes' own address, which its row of Documents' list and a chip that names it open. */
const NOTES_ADDRESS = buildIssuePath({ key: KEY, kind: "artifact", slug: NOTES.slug });

/** The chip that names the notes in a row's words, as the row draws it (`RowText.tsx`). */
const NOTES_CHIP = `a[href="${NOTES_ADDRESS}"]`;

/** The issue's two threads, the second a head with no reply, which cites the notes: a row that is
 *  no folded thread draws its words whole, the notes as a chip. */
const CHIP_THREADS = TWO_THREADS.flatMap((comment) =>
  comment.id === "reply-b"
    ? []
    : comment.id === "head-b"
      ? [
          {
            ...comment,
            body: `The words of head-b cite dispatch://${KEY}/artifact/${NOTES.slug} too`,
          },
        ]
      : [comment]
);

/** One typed block of a document as the blocks read answers it, with the statement it serves. */
function typedBlock(id: string, type: string, serves?: string): ArtifactBlock {
  return {
    attributes: serves === undefined ? [] : [{ kind: "block[]", name: "serves", value: serves }],
    from: 0,
    id,
    references: { asks: 0, comments: 0 },
    to: 1,
    token: id,
    type,
  };
}

const BLOCKS: Readonly<Record<string, readonly ArtifactBlock[]>> = {
  "artifact-spec": [typedBlock(STATEMENT, "requirement")],
  [PLAN_DOCUMENT.id]: [typedBlock(PLAN_TASK, "plan_task", specRef(STATEMENT))],
  [VERIFICATION_DOCUMENT.id]: [typedBlock(CHECK, "check", specRef(STATEMENT))],
};

/** Where the router is: the address, how it got there, the location's state and the following;
 *  Back; a link to a comment of the issue; and a link that opens Documents' list with the address
 *  it is pressed at as the address the list goes back to (the location's `outline`, which the
 *  control that opens the list hands over). */
function Where(): ReactNode {
  const { pathname, search, state } = useLocation();
  const navigate = useNavigate();
  return (
    <>
      <output
        data-following={useFollowing()}
        data-move={useNavigationType()}
        data-state={JSON.stringify(state)}
        data-testid="where"
      >
        {`${pathname}${search}`}
      </output>
      <button onClick={() => navigate(-1)} type="button">
        Back
      </button>
      <Link to={`/issues/${KEY}/conversation?comment=head-b`}>The second thread</Link>
      <Link state={{ outline: `${pathname}${search}` }} to={DOCUMENTS}>
        Open Documents
      </Link>
    </>
  );
}

/** Where the page stood at each commit that drew the outline, read in a layout effect after the
 *  stage's own: where the browser paints the outline on its first paint after the commit. */
let outlinePaints: number[] = [];

function PaintProbe(): null {
  // Drawn again at each move of the router, in the commit the stage is drawn again in.
  useLocation();
  useLayoutEffect(() => {
    if (document.querySelector("[data-issue-body]") !== null) outlinePaints.push(window.scrollY);
  });
  return null;
}

/** The view row over the stage at the address the router holds, under the page's own
 *  providers, inside the page's root, as the page's root draws them. */
function Page(): ReactNode {
  const { pathname, search } = useLocation();
  const route = parseIssuePath(pathname, search);
  if (route === undefined) throw new Error(`${pathname} names no route`);
  return (
    <div data-issue-page="">
      <ProvidersSlot issue={ISSUE} route={route}>
        <ViewRowSlot issue={ISSUE} route={route} />
        <StageSlot issue={ISSUE} route={route} />
        <PaintProbe />
        <Where />
      </ProvidersSlot>
    </div>
  );
}

/** The shell's Details drawer as it draws the new page's frame (`app.tsx`), closed on the load,
 *  with a control that opens it, around the page. */
function WithDetails({ children }: { children: ReactNode }): ReactNode {
  const [open, setOpen] = useState(false);
  const [page, setPage] = useState<MarginDrawerPage | undefined>(undefined);
  const drawer = useMemo<MarginDrawer>(() => ({ open, page, setOpen, setPage }), [open, page]);
  return (
    <MarginSlotProvider drawer={drawer} drawsSlot>
      <button onClick={() => setOpen(true)} type="button">
        Open Details
      </button>
      {children}
      <Margin />
    </MarginSlotProvider>
  );
}

/** The rooms of the page's documents, as the last page drawn opens them (`drawPage`). */
let rooms: FakeDocumentRuntime | undefined;

/** The document runtime the page's editors are made by: an editable that takes a caret, which
 *  draws the spec's statement as its block, as a row's editor finds the node's block to put the
 *  caret in. Its view takes a plugin as ProseMirror's does, which is how the row holds every edit
 *  to the node's block before it puts the caret in (`node-caret.ts`'s `holdEditsToBlock`). */
function editableRuntime(fake: FakeDocumentRuntime): DocumentRuntimeValue {
  return {
    ...fake.runtime,
    createEditor: async (root, options) => {
      const handle = await fake.runtime.createEditor(root, options);
      root.contentEditable = "true";
      const block = document.createElement("p");
      block.dataset.blockId = STATEMENT;
      block.textContent = "The page draws the plan.";
      root.replaceChildren(block);
      const view = handle.view as unknown as { state: object; updateState: () => void };
      Object.assign(view.state, { plugins: [], reconfigure: () => view.state });
      view.updateState = () => {};
      return handle;
    },
  };
}

/** The window as happy-dom draws it, whose width every media query of the page is answered
 *  from: the outline's own include a `not all and` that no stub reads. */
const testWindow = window as unknown as {
  happyDOM: { setViewport(size: { width: number }): void };
  innerWidth: number;
};
let widthBefore = testWindow.innerWidth;
let restoreReads: { mockRestore(): void }[] = [];

beforeEach(() => {
  outlinePaints = [];
  widthBefore = testWindow.innerWidth;
  testWindow.happyDOM.setViewport({ width: DESKTOP_WIDTH });
  restoreReads = [
    ...stubReads(),
    spyOn(api, "getIssueCoverage").mockResolvedValue({
      gaps: [],
      has_plan: true,
      issue: KEY,
      requirements: [
        {
          block_id: STATEMENT,
          checks: 1,
          gaps: [],
          plan_tasks: 1,
          ref: specRef(STATEMENT),
          text: "The page draws the plan.",
        },
      ],
      spec_artifact_id: "artifact-spec",
    }),
    spyOn(api, "getArtifactBlocks").mockImplementation(async (id: string) => [
      ...(BLOCKS[id] ?? []),
    ]),
    spyOn(api, "getIssueResults").mockResolvedValue({
      checks: [check({ block_id: CHECK })],
      issue: KEY,
      verification_artifact_id: VERIFICATION_DOCUMENT.id,
    }),
    spyOn(api, "listComments").mockResolvedValue([...TWO_THREADS]),
    spyOn(api, "getArtifactText").mockResolvedValue({
      markdown: "The notes",
      token: "sha256:notes",
      version: null,
    }),
  ];
  // What an opened document reads beside its text: its record, its blocks, the issue's
  // decisions and the reader's read mark, and what references it.
  answerRequests(
    artifactRecordHandler((id) => (ISSUE.artifacts ?? []).find((each) => each.id === id)),
    http.get("/api/v1/artifacts/:id/blocks", () => Response.json([])),
    http.get("/api/v1/issues/:key/references", () =>
      Response.json({
        linked_from: [],
        links: [],
        members: [],
        truncated: false,
      } satisfies IssueReferences)
    ),
    http.get("/api/v1/references", ({ request }) =>
      Response.json({
        edges: [],
        node: graphNode({
          id: new URL(request.url).searchParams.get("to") ?? "",
          kind: "artifact",
        }),
      } satisfies GraphReferences)
    ),
    ...decisionsAndReadMarkHandlers(),
    // What Details' drawer reads of the issue and the reader.
    http.get(`/api/v1/issues/${KEY}`, () => Response.json(ISSUE)),
    http.get("/api/v1/inbox", () => Response.json([])),
    http.get("/api/v1/me/state", () => Response.json({}))
  );
});

afterEach(() => {
  testWindow.happyDOM.setViewport({ width: widthBefore });
  for (const read of restoreReads) read.mockRestore();
  window.scrollTo({ top: 0 });
});

/** The page at `address`, with `state` in its location where one is given, at 1440 px, inside
 *  the shell's Details drawer where `details` says so. */
function drawPage(
  address: string,
  {
    client = new QueryClient({ defaultOptions: { queries: { retry: false } } }),
    details = false,
    state,
  }: { client?: QueryClient; details?: boolean; state?: unknown } = {}
): RenderResult {
  const [pathname, search = ""] = address.split("?");
  const page = details ? (
    <WithDetails>
      <Page />
    </WithDetails>
  ) : (
    <Page />
  );
  rooms = fakeDocumentRuntime({ text: "The notes" });
  return render(
    <MemoryRouter initialEntries={[{ pathname, search: search === "" ? "" : `?${search}`, state }]}>
      <AppProviders client={client}>
        <DocumentRuntime.Provider value={editableRuntime(rooms)}>{page}</DocumentRuntime.Provider>
      </AppProviders>
    </MemoryRouter>
  );
}

/** Where the router is, as `Where` draws it. */
function where(): HTMLElement {
  return screen.getByTestId("where");
}

/** The address the router is at. */
function address(): string {
  return where().textContent ?? "";
}

/** The words box, open. */
function box(): HTMLElement {
  return screen.getByRole("searchbox", { name: "Filter" });
}

/** The row of `node` in the body `body` marks, once it is drawn. */
async function rowOf(view: RenderResult, body: string, node: string): Promise<HTMLElement> {
  const selector = `${body} ${nodeRowSelectorOf(node)}`;
  return waitFor(() => {
    const row = view.container.querySelector<HTMLElement>(selector);
    if (row === null) throw new Error(`no row for ${node} in ${body}`);
    return row;
  });
}

/** Puts the focus on `element`, as a key that moves to it does. */
async function focus(element: HTMLElement): Promise<void> {
  await act(async () => element.focus());
}

/** The reader scrolls the page to `top`: the browser moves it, then says so. */
function scrollPage(top: number): void {
  act(() => {
    window.scrollTo({ top });
    window.dispatchEvent(new Event("scroll"));
  });
}

/** The view on, by the view control that is pressed. */
function pressed(): string | null | undefined {
  return document.querySelector('[aria-pressed="true"]')?.textContent;
}

/** Details' drawer, open: closed, the shell keeps it drawn, hidden. */
const OPEN_DETAILS = `aside[${DETAILS_ATTRIBUTE}]:not([hidden])`;

// ---- ST-2, Escape's order

test("a caret in a row's editor: one Escape leaves the caret for its row, and the words stay", async () => {
  const user = userEvent.setup();
  const view = drawPage(`/issues/${KEY}?q=${WORDS}`);
  try {
    const row = await rowOf(view, "[data-issue-body]", STATEMENT_NODE);
    await focus(row);
    await user.keyboard("{Enter}");
    // The document's room admits the page and syncs, so the row's editor is made.
    await waitFor(() => expect(rooms?.connections.length).toBeGreaterThan(0));
    await act(async () => rooms?.callbacks.at(-1)?.onAdmission(false));
    await act(async () => rooms?.callbacks.at(-1)?.onSynced());
    const editable = await waitFor(
      () => {
        const active = document.activeElement as HTMLElement | null;
        if (active === null || !active.isContentEditable) throw new Error("no caret in the row");
        return active;
      },
      { timeout: 10_000 }
    );
    expect(row.contains(editable), "the caret is in no editor of the row").toBe(true);
    await user.keyboard("{Escape}");
    await waitFor(() => expect(focusedRow()).toBe(STATEMENT_NODE));
    expect((document.activeElement as HTMLElement).isContentEditable).toBe(false);
    expect(address(), "the Escape that left the caret cleared the words").toBe(
      `/issues/${KEY}?q=${WORDS}`
    );
  } finally {
    view.unmount();
  }
});

/** The second thread's head cites the notes, so its row draws them as a chip (`RowText.tsx`). */
function chipInHeadB(): void {
  restoreReads.push(spyOn(api, "listComments").mockResolvedValue([...CHIP_THREADS]));
}

/** The chip that names the notes in the outline's row of the second thread's head, once the
 *  outline draws it. */
async function chipOf(view: RenderResult): Promise<HTMLElement> {
  const row = await rowOf(view, "[data-issue-body]", "head-b");
  return waitFor(() => {
    const chip = row.querySelector<HTMLElement>(NOTES_CHIP);
    if (chip === null) throw new Error("the second thread's head draws no chip of the notes");
    return chip;
  });
}

/** Each row of Documents' list, by its link to its document. */
const LIST_ROWS = `[${DOCUMENTS_LIST_ATTRIBUTE}] [${DOCUMENT_OPEN_ATTRIBUTE}]`;

/** The rows of Documents' list, by each row's link to its document, once the list draws them. */
async function listLinks(): Promise<HTMLElement[]> {
  return waitFor(() => {
    const links = [...document.querySelectorAll<HTMLElement>(LIST_ROWS)];
    if (links.length === 0) throw new Error("Documents' list draws no row");
    return links;
  });
}

/** The opened document's own box, which holds the focus as no control of it does. */
async function focusOpenedDocument(): Promise<void> {
  const heading = await screen.findByRole("heading", { name: NOTES.name });
  await focus(heading.closest<HTMLElement>('[tabindex="-1"]') ?? heading);
}

test("a document open that Documents' list opened: one Escape closes it, back to the list with the focus on the document's row", async () => {
  const user = userEvent.setup();
  const view = drawPage(DOCUMENTS);
  try {
    const notes = (await listLinks()).find((link) => link.textContent === NOTES.name);
    if (notes === undefined) throw new Error("Documents' list draws no row of the notes");
    await user.click(notes);
    await waitFor(() => expect(address()).toBe(NOTES_ADDRESS));
    await focusOpenedDocument();
    await user.keyboard("{Escape}");
    await waitFor(() => expect(address()).toBe(DOCUMENTS));
    expect(where(), "the way back to the list is no push").toHaveAttribute("data-move", "PUSH");
    expect(screen.queryByRole("heading", { name: NOTES.name })).toBeNull();
    await waitFor(() =>
      expect(document.activeElement?.textContent, "the focus is not on the notes' row").toBe(
        NOTES.name
      )
    );
    expect(document.activeElement?.closest(`[${DOCUMENTS_LIST_ATTRIBUTE}]`)).not.toBeNull();
  } finally {
    view.unmount();
  }
});

test("a document open that a chip opened: one Escape closes it, back to the view and the row that hold the chip", async () => {
  chipInHeadB();
  const user = userEvent.setup();
  const pressedAt = `/issues/${KEY}/conversation?comment=head-b`;
  const view = drawPage(pressedAt);
  try {
    await user.click(await chipOf(view));
    await waitFor(() => expect(address()).toBe(NOTES_ADDRESS));
    await focusOpenedDocument();
    await user.keyboard("{Escape}");
    await waitFor(() =>
      expect(address(), "the chip's document went back elsewhere").toBe(pressedAt)
    );
    expect(where()).toHaveAttribute("data-move", "PUSH");
    expect(screen.queryByRole("heading", { name: NOTES.name })).toBeNull();
    expect(pressed()).toBe("Conversation");
    await chipOf(view);
  } finally {
    view.unmount();
  }
});

test("Escape from a document a chip opened puts the focus on the chip drawn again", async () => {
  chipInHeadB();
  const user = userEvent.setup();
  // The address names the chip's row, whose own landing comes as the outline is drawn again.
  const view = drawPage(`/issues/${KEY}/conversation?comment=head-b`);
  try {
    const pressedChip = await chipOf(view);
    await user.click(pressedChip);
    await focusOpenedDocument();
    await user.keyboard("{Escape}");
    const drawnAgain = await waitFor(() => {
      const active = document.activeElement as HTMLElement | null;
      expect(active?.matches(NOTES_CHIP), "the focus is not on the chip").toBe(true);
      return active as HTMLElement;
    });
    expect(drawnAgain, "the chip is the element the follow took off the page").not.toBe(
      pressedChip
    );
    expect(drawnAgain.closest(nodeRowSelectorOf("head-b"))).not.toBeNull();
    // The landing of the address the way back opened leaves it there.
    for (let frame = 0; frame < 5; frame += 1) await act(nextFrame);
    expect(document.activeElement, "a landing took the focus off the chip").toBe(drawnAgain);
  } finally {
    view.unmount();
  }
});

test("a document open from its address on a cold load: one Escape closes it, back to Documents' list with the focus on its first row", async () => {
  const user = userEvent.setup();
  const view = drawPage(NOTES_ADDRESS);
  try {
    await focusOpenedDocument();
    await user.keyboard("{Escape}");
    await waitFor(() => expect(address()).toBe(DOCUMENTS));
    const [first] = await listLinks();
    await waitFor(() =>
      expect(document.activeElement, "the focus is not on the list's first row").toBe(first ?? null)
    );
    // The issue's spec is the list's first row.
    expect(first?.textContent).toBe(SPEC_SLUG);
  } finally {
    view.unmount();
  }
});

test("the focus in Filter files and documents, words in it: one Escape clears its words, and the list stays", async () => {
  const user = userEvent.setup();
  const view = drawPage(DOCUMENTS);
  try {
    await listLinks();
    const filter = screen.getByRole("searchbox", { name: CONTROL_LABELS.filter_files });
    await user.type(filter, NOTES.name);
    await waitFor(() => expect(document.querySelectorAll(LIST_ROWS)).toHaveLength(1));
    await user.keyboard("{Escape}");
    await waitFor(() => expect(filter).toHaveValue(""));
    expect(filter).toHaveFocus();
    expect(await listLinks()).toHaveLength(ISSUE.artifacts?.length ?? 0);
    expect(address(), "the Escape that cleared the words left the list too").toBe(DOCUMENTS);
  } finally {
    view.unmount();
  }
});

test("Documents' list with an upload staged: one Escape cancels the upload, as Cancel does, and sends nothing", async () => {
  const uploadArtifact = spyOn(api, "uploadArtifact");
  restoreReads.push(uploadArtifact);
  const user = userEvent.setup();
  const view = drawPage(DOCUMENTS);
  try {
    await listLinks();
    const staged = new File(["# Staged\n"], "staged.md", { type: "text/markdown" });
    await user.upload(screen.getByLabelText(CONTROL_LABELS.upload_file), staged);
    expect(screen.getByText("staged.md")).toBeInTheDocument();
    // Upload's pick takes Upload away, and the focus with it.
    await act(async () => (document.activeElement as HTMLElement | null)?.blur());
    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByText("staged.md")).toBeNull());
    expect(screen.queryByRole("textbox", { name: "Summary (optional)" })).toBeNull();
    expect(uploadArtifact).not.toHaveBeenCalled();
    expect(address(), "the Escape that cancelled the upload left the list too").toBe(DOCUMENTS);
  } finally {
    view.unmount();
  }
});

test("Documents' list reached from another control: one Escape returns to that control at its address, the focus where it was", async () => {
  const user = userEvent.setup();
  const view = drawPage(`/issues/${KEY}/spec`);
  try {
    await focus(await rowOf(view, "[data-spec-body]", STATEMENT_NODE));
    await user.click(screen.getByRole("link", { name: "Open Documents" }));
    await waitFor(() => expect(address()).toBe(DOCUMENTS));
    const [first] = await listLinks();
    if (first !== undefined) await focus(first);
    await user.keyboard("{Escape}");
    await waitFor(() => expect(address()).toBe(`/issues/${KEY}/spec`));
    expect(pressed()).toBe("Spec");
    await waitFor(() => expect(focusedRow()).toBe(STATEMENT_NODE));
  } finally {
    view.unmount();
  }
});

test("Documents' list, the first view of the load: one Escape returns to Conversation", async () => {
  const user = userEvent.setup();
  const view = drawPage(DOCUMENTS);
  try {
    const [first] = await listLinks();
    if (first !== undefined) await focus(first);
    await user.keyboard("{Escape}");
    await waitFor(() => expect(address()).toBe(CONVERSATION));
    expect(pressed()).toBe("Conversation");
    await rowOf(view, "[data-issue-body]", "head-a");
  } finally {
    view.unmount();
  }
});

test("the focus in the words box: one Escape leaves the box for the row it was on, else its Filter control or the stage's tab stop, and keeps the words", async () => {
  const user = userEvent.setup();
  const view = drawPage(`/issues/${KEY}/spec`);
  try {
    // From a row, by `t f`: the words typed stay, the box stays open, and the row has the focus.
    const row = await rowOf(view, "[data-spec-body]", STATEMENT_NODE);
    await focus(row);
    await user.keyboard("tf");
    await waitFor(() => expect(box()).toHaveFocus());
    await user.type(box(), WORDS);
    await waitFor(() => expect(address()).toBe(`/issues/${KEY}/spec?q=${WORDS}`));
    await user.keyboard("{Escape}");
    await waitFor(() => expect(focusedRow()).toBe(STATEMENT_NODE));
    expect(address(), "leaving the box cleared its words").toBe(`/issues/${KEY}/spec?q=${WORDS}`);
    expect(box()).toHaveValue(WORDS);
    expect(pressed()).toBe("Spec");

    // Emptied, the box shuts into its control as the focus goes back to the row.
    await user.click(box());
    await user.clear(box());
    await waitFor(() => expect(address()).toBe(`/issues/${KEY}/spec`));
    await user.keyboard("{Escape}");
    await waitFor(() => expect(focusedRow()).toBe(STATEMENT_NODE));
    expect(screen.queryByRole("searchbox", { name: "Filter" })).toBeNull();

    // With no row to go back to, an empty box shuts into its Filter control, which takes the
    // focus.
    await act(async () => (document.activeElement as HTMLElement | null)?.blur());
    await user.click(screen.getByRole("button", { name: "Filter" }));
    await waitFor(() => expect(box()).toHaveFocus());
    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.getByRole("button", { name: "Filter" })).toHaveFocus());
    expect(pressed()).toBe("Spec");

    // With no row to go back to, a box that holds words stays open, and the focus goes to the
    // stage's own tab stop, the view's tab-stop row: never to the page's body.
    await act(async () => (document.activeElement as HTMLElement | null)?.blur());
    await user.click(screen.getByRole("button", { name: "Filter" }));
    await waitFor(() => expect(box()).toHaveFocus());
    await user.type(box(), WORDS);
    await waitFor(() => expect(address()).toBe(`/issues/${KEY}/spec?q=${WORDS}`));
    await user.keyboard("{Escape}");
    await waitFor(() =>
      expect(document.activeElement, "leaving the box left the focus on the page's body").not.toBe(
        document.body
      )
    );
    const stop = await rowOf(view, "[data-spec-body]", STATEMENT_NODE);
    expect(document.activeElement, "the focus did not go to the view's tab stop").toBe(stop);
    expect(stop).toHaveAttribute("tabindex", "0");
    expect(box()).toHaveValue(WORDS);
    expect(address()).toBe(`/issues/${KEY}/spec?q=${WORDS}`);
  } finally {
    view.unmount();
  }
});

test("words in ?q=, the focus outside the box: one Escape clears the words by the box's own replace and shuts the box, and the view and its following stay", async () => {
  const user = userEvent.setup();
  const view = drawPage(`/issues/${KEY}/spec?q=${WORDS}`, {
    state: { outline: `/issues/${KEY}/plan`, waitingPress: 1 },
  });
  try {
    const row = await rowOf(view, "[data-spec-body]", STATEMENT_NODE);
    const body = view.container.querySelector("[data-spec-body]");
    const before = where().dataset.following;
    await focus(row);
    await user.keyboard("{Escape}");
    await waitFor(() => expect(address()).toBe(`/issues/${KEY}/spec`));
    expect(where()).toHaveAttribute("data-move", "REPLACE");
    expect(JSON.parse(where().dataset.state ?? "null")).toEqual({
      outline: `/issues/${KEY}/plan`,
      words: before,
    });
    expect(where().dataset.following, "clearing the words began a new following").toBe(before);
    expect(
      view.container.querySelector("[data-spec-body]"),
      "clearing the words drew the view anew"
    ).toBe(body);
    expect(pressed()).toBe("Spec");
    expect(screen.queryByRole("searchbox", { name: "Filter" })).toBeNull();
    expect(screen.getByRole("button", { name: "Filter" })).toBeVisible();
    expect(focusedRow(), "clearing the words took the reader off their row").toBe(STATEMENT_NODE);
  } finally {
    view.unmount();
  }
});

// Each view is drawn as a load with the page's reads in hand draws it, Back to its address among
// them: its rows and their Escape in the commit that draws the stage's own.
test("a view on: one Escape turns every view off, and the outline draws the node the reader was on in the view, focused", async () => {
  const user = userEvent.setup();
  for (const [name, preset, body, node, inside] of [
    ["Spec", "spec", "[data-spec-body]", STATEMENT_NODE, undefined],
    ["Plan", "plan", "[data-plan-body]", TASK_NODE, undefined],
    ["Verification", "verification", "[data-verification-body]", CHECK_NODE, undefined],
    // A control inside a row of the Verification view, which binds no Escape of its own.
    [
      "Verification",
      "verification",
      "[data-verification-body]",
      CHECK_NODE,
      "[data-verification-check-words]",
    ],
  ] as const) {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const reads = drawPage(`/issues/${KEY}/${preset}`, { client });
    await rowOf(reads, body, node);
    reads.unmount();
    const view = drawPage(`/issues/${KEY}/${preset}`, { client });
    try {
      const row = await rowOf(view, body, node);
      await focus(inside === undefined ? row : (row.querySelector<HTMLElement>(inside) ?? row));
      expect(pressed()).toBe(name);
      await user.keyboard("{Escape}");
      await waitFor(() =>
        expect(address(), `Escape on the ${name} view's row left the view on`).toBe(CONVERSATION)
      );
      await waitFor(() =>
        expect(
          document.activeElement?.closest(`[data-issue-body] ${nodeRowSelectorOf(node)}`),
          `the outline does not draw the ${name} view's node focused`
        ).not.toBeNull()
      );
    } finally {
      view.unmount();
    }
  }
});

test("a row of the outline focused, nothing above: one Escape leaves the outline", async () => {
  const user = userEvent.setup();
  const view = drawPage(CONVERSATION);
  try {
    const row = await rowOf(view, "[data-issue-body]", "head-a");
    await focus(row);
    await user.keyboard("{Escape}");
    await waitFor(() => expect(focusedRow()).toBeUndefined());
    expect(document.activeElement).toBe(document.body);
    expect(address()).toBe(CONVERSATION);
    expect(where()).toHaveAttribute("data-move", "POP");
  } finally {
    view.unmount();
  }
});

test("Details open: Details' own Escape closes it first, and the view stays on until the next", async () => {
  const user = userEvent.setup();
  const view = drawPage(`/issues/${KEY}/spec`, { details: true });
  try {
    const row = await rowOf(view, "[data-spec-body]", STATEMENT_NODE);
    await user.click(screen.getByRole("button", { name: "Open Details" }));
    await waitFor(() => expect(document.querySelector(OPEN_DETAILS)).not.toBeNull());
    await focus(row);
    await user.keyboard("{Escape}");
    await waitFor(() => expect(document.querySelector(OPEN_DETAILS)).toBeNull());
    expect(address(), "the Escape that closed Details turned the view off too").toBe(
      `/issues/${KEY}/spec`
    );
    expect(pressed()).toBe("Spec");
    await user.keyboard("{Escape}");
    await waitFor(() => expect(address()).toBe(CONVERSATION));
  } finally {
    view.unmount();
  }
});

// ---- ST-5, the outline's scroll place

/** Turns the Spec view on by its control, and lets the browser clamp the page to the view's
 *  shorter height, which it reports as a scroll. */
async function specOn(user: UserEvent): Promise<void> {
  await user.click(screen.getByRole("button", { name: "Spec" }));
  await waitFor(() => expect(pressed()).toBe("Spec"));
  scrollPage(0);
}

test("the reader scrolls the outline: the place is recorded for the following it was scrolled in", async () => {
  const user = userEvent.setup();
  const view = drawPage(CONVERSATION);
  try {
    await rowOf(view, "[data-issue-body]", "head-a");
    scrollPage(200);
    scrollPage(480);
    await specOn(user);
    await user.click(screen.getByRole("button", { name: "Conversation" }));
    await waitFor(() => expect(address()).toBe(CONVERSATION));
    expect(window.scrollY, "the outline's last place was not put back").toBe(480);
    // A place of this following, and Back past the view to the first one: each its own.
    scrollPage(120);
    await user.click(screen.getByRole("button", { name: "Back" }));
    await waitFor(() => expect(address()).toBe(`/issues/${KEY}/spec`));
    scrollPage(0);
    await user.click(screen.getByRole("button", { name: "Back" }));
    await waitFor(() => expect(address()).toBe(CONVERSATION));
    expect(window.scrollY, "Back put the later following's place back").toBe(480);
  } finally {
    view.unmount();
  }
});

test("a view press or chord: the outline's place is kept while the view draws", async () => {
  const user = userEvent.setup();
  const view = drawPage(CONVERSATION);
  try {
    await rowOf(view, "[data-issue-body]", "head-a");
    scrollPage(480);
    await specOn(user);
    // The reader scrolls the view, and a chord takes them to another view, which they scroll.
    scrollPage(900);
    await user.keyboard("tp");
    await waitFor(() => expect(pressed()).toBe("Plan"));
    scrollPage(60);
    await user.keyboard("tc");
    await waitFor(() => expect(address()).toBe(CONVERSATION));
    expect(window.scrollY, "a view's scroll was taken for the outline's place").toBe(480);
  } finally {
    view.unmount();
  }
});

test("Conversation press, t c, or Escape off the view: the outline's place is put back before the browser paints", async () => {
  const user = userEvent.setup();
  for (const [way, back] of [
    [
      "the Conversation control",
      () => user.click(screen.getByRole("button", { name: "Conversation" })),
    ],
    ["t c", () => user.keyboard("tc")],
    ["Escape", () => user.keyboard("{Escape}")],
  ] as const) {
    const view = drawPage(CONVERSATION);
    try {
      await rowOf(view, "[data-issue-body]", "head-a");
      scrollPage(480);
      await specOn(user);
      outlinePaints = [];
      await back();
      await waitFor(() => expect(address()).toBe(CONVERSATION));
      expect(outlinePaints[0], `the outline was painted away from its place, back by ${way}`).toBe(
        480
      );
      expect(window.scrollY).toBe(480);
    } finally {
      view.unmount();
      window.scrollTo({ top: 0 });
    }
  }
});

test("Back to the outline address: the outline's place is put back", async () => {
  const user = userEvent.setup();
  // The browser's Back from a view.
  const view = drawPage(CONVERSATION);
  try {
    await rowOf(view, "[data-issue-body]", "head-a");
    scrollPage(480);
    await specOn(user);
    await user.click(screen.getByRole("button", { name: "Back" }));
    await waitFor(() => expect(address()).toBe(CONVERSATION));
    expect(window.scrollY).toBe(480);
  } finally {
    view.unmount();
    window.scrollTo({ top: 0 });
  }
  // The way back of a document a chip opened, to the address the chip was pressed at, which
  // names a node.
  chipInHeadB();
  const named = drawPage(`/issues/${KEY}/conversation?comment=head-b`);
  try {
    await waitFor(() => expect(focusedRow()).toBe("head-b"));
    scrollPage(480);
    await user.click(await chipOf(named));
    await screen.findByRole("heading", { name: NOTES.name });
    scrollPage(0);
    await user.keyboard("{Escape}");
    await waitFor(() => expect(address()).toBe(`/issues/${KEY}/conversation?comment=head-b`));
    expect(window.scrollY).toBe(480);
  } finally {
    named.unmount();
  }
});

test("a new address, a link: the outline's place is not put back, and the address lands by its own rules", async () => {
  const user = userEvent.setup();
  const view = drawPage(CONVERSATION);
  try {
    await rowOf(view, "[data-issue-body]", "head-a");
    scrollPage(480);
    await specOn(user);
    outlinePaints = [];
    await user.click(screen.getByRole("link", { name: "The second thread" }));
    await waitFor(() => expect(address()).toBe(`/issues/${KEY}/conversation?comment=head-b`));
    await waitFor(() => expect(focusedRow()).toBe("head-b"));
    expect(outlinePaints[0], "the link's outline was painted at the old place").toBe(0);
    expect(window.scrollY).toBe(0);
  } finally {
    view.unmount();
  }
});

/** The outline's rows, in the order the page draws them. */
function outlineRows(): HTMLElement[] {
  return [...document.querySelectorAll<HTMLElement>("[data-issue-body] [data-node-row]")];
}

/** Lays the outline's rows out down the page where happy-dom lays nothing out: each 80 px tall,
 *  one every 200 px from 100 px down, read at the place the page stands at. Every other box is
 *  empty. */
function layOutRows(): { mockRestore(): void } {
  return spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement
  ) {
    const index = outlineRows().indexOf(this);
    return index === -1
      ? new DOMRect()
      : new DOMRect(0, 100 + 200 * index - window.scrollY, 600, 80);
  });
}

/** The spec with no statement, so the Spec view draws no row and sets no node of its own: the
 *  node the outline then draws focused is the one the reader was on before the view. */
function specWithNoStatement(): void {
  restoreReads.push(
    spyOn(api, "getIssueCoverage").mockResolvedValue({
      gaps: [],
      has_plan: true,
      issue: KEY,
      requirements: [],
      spec_artifact_id: "artifact-spec",
    }),
    spyOn(api, "getArtifactBlocks").mockImplementation(async (id: string) =>
      id === "artifact-spec" ? [] : [...(BLOCKS[id] ?? [])]
    )
  );
}

test("t s then t c: the outline's place is put back and the row the reader was on holds the focus", async () => {
  specWithNoStatement();
  const user = userEvent.setup();
  const view = drawPage(CONVERSATION);
  const boxes = layOutRows();
  try {
    await rowOf(view, "[data-issue-body]", "head-b");
    // The third row, at 500 px down: the place below shows it whole, and the second row above it.
    const on = outlineRows()[2] as HTMLElement;
    await focus(on);
    scrollPage(300);
    await user.keyboard("ts");
    await waitFor(() => expect(pressed()).toBe("Spec"));
    scrollPage(0);
    await user.keyboard("tc");
    await waitFor(() => expect(address()).toBe(CONVERSATION));
    await waitFor(() =>
      expect(focusedRow(), "the row the reader was on does not hold the focus").toBe(
        on.dataset.nodeId
      )
    );
    expect(window.scrollY, "the outline's place was not put back").toBe(300);
  } finally {
    boxes.mockRestore();
    view.unmount();
  }
});

test("a reader who scrolled away from the focused row: a round trip puts the place back and the first row on screen takes the focus", async () => {
  specWithNoStatement();
  const user = userEvent.setup();
  const view = drawPage(CONVERSATION);
  const boxes = layOutRows();
  try {
    await rowOf(view, "[data-issue-body]", "head-b");
    const [first, second] = outlineRows().map((row) => row.dataset.nodeId);
    // The reader is on the first row, at 100 px down, and scrolls it away: at 300 px the second
    // row, at 300 px down, is the first the page shows whole.
    await focus(outlineRows()[0] as HTMLElement);
    expect(focusedRow()).toBe(first);
    scrollPage(300);
    await specOn(user);
    await user.click(screen.getByRole("button", { name: "Conversation" }));
    await waitFor(() => expect(address()).toBe(CONVERSATION));
    await waitFor(() =>
      expect(focusedRow(), "the first row on screen does not hold the focus").toBe(second)
    );
    expect(window.scrollY, "the outline's place was not put back").toBe(300);
  } finally {
    boxes.mockRestore();
    view.unmount();
  }
});

// ST-5 and the plan's preset table: Documents' list is drawn in the outline's place, so the
// outline's scroll place is neither put back on the list when it opens nor taken from a scroll
// of the list, and Conversation after it puts back the outline's own place.
test("the outline's scroll place is neither taken nor put back on Documents' list", async () => {
  const user = userEvent.setup();
  const view = drawPage(CONVERSATION);
  try {
    await rowOf(view, "[data-issue-body]", "head-a");
    scrollPage(480);
    await specOn(user);
    await user.click(screen.getByRole("link", { name: "Open Documents" }));
    await waitFor(() => expect(address()).toBe(DOCUMENTS));
    await listLinks();
    expect(window.scrollY, "the outline's place was put back on Documents' list").toBe(0);
    scrollPage(900);
    await user.click(screen.getByRole("button", { name: "Conversation" }));
    await waitFor(() => expect(address()).toBe(CONVERSATION));
    expect(window.scrollY, "a scroll of Documents' list was taken for the outline's place").toBe(
      480
    );
  } finally {
    view.unmount();
  }
});
