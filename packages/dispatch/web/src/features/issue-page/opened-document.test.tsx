import { beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClientProvider } from "@tanstack/react-query";
import { act, type RenderResult, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http } from "msw";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation, useNavigate, useNavigationType } from "react-router-dom";

import { fakeDocumentRuntime } from "../../__tests__/document-runtime";
import { graphNode } from "../../__tests__/graph-fixture";
import { answerRequests } from "../../__tests__/network";
import {
  answerTextReadsWithTheSeededText,
  artifact,
  artifactRecordHandler,
  createQueryClient,
  documentSurfaceHandlers,
} from "../../__tests__/proof-document";
import { ApiError, api } from "../../api/client";
import type {
  Artifact,
  GraphEdge,
  GraphReferences,
  IssueDetails,
  IssueReferences,
  Version,
} from "../../api/types";
import { KEY } from "../decision-card/decision-fixture";
import { DocumentRuntime } from "../doc/runtime";
import { MarginProvider } from "../margin/margin-context";
import { DESKTOP_WIDTH } from "../margin/margin-fixture";
import { buildIssuePath, parseIssuePath } from "../refs/routes";
import { DOCUMENT_OPEN_ATTRIBUTE, DOCUMENTS_LIST_ATTRIBUTE } from "./DocumentsList";
import { artifact as issueArtifact, issueDetails } from "./decisions-filter/decisions-fixtures";
import { NoTooltips } from "./NoTooltips";
import { OpenedDocument } from "./OpenedDocument";
import { TWO_THREADS } from "./outline-fixtures";
import { CONTROL_LABELS, NODE_REFERENCES_WORDS } from "./page-words";
import { SHOW_MORE, showMoreName } from "./show-more-fixture";
import { ProvidersSlot } from "./slots/providers";
import { StageSlot } from "./slots/stage";
import { AppProviders, stubReads } from "./slots/stage-fixtures";
import { ViewRowSlot } from "./slots/view-row";
import { nodeRowSelectorOf } from "./tree-focus";

/**
 * A document opened in place of the outline (part N's R2), drawn as the new issue page draws it:
 * inside `NoTooltips`, with the page's document reads answered and a fake room for its editor;
 * and, for its way back, on the page's own stage, inside the page's root, at the address the
 * router holds, with Documents' list and the outline it goes back to.
 */

answerTextReadsWithTheSeededText();

/** The document's own reads, and what references it and the issue, which nothing does here. */
beforeEach(() => {
  answerRequests(
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
    ...documentSurfaceHandlers()
  );
});

/** `opened`, the issue's spec where none is given, opened at its own address under
 *  `NoTooltips`. */
function renderOpened(opened: Artifact = artifact) {
  const client = createQueryClient();
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const fake = fakeDocumentRuntime({ text: "The live document" });
  return render(
    <MemoryRouter
      initialEntries={[buildIssuePath({ key: "CORE-1", kind: "artifact", slug: opened.slug })]}
    >
      <QueryClientProvider client={client}>
        <DocumentRuntime.Provider value={fake.runtime}>
          <MarginProvider>
            <NoTooltips>
              <OpenedDocument
                artifact={opened}
                artifactRoute={{ key: "CORE-1", kind: "artifact", slug: opened.slug }}
                highlightTerm={undefined}
                isClosed={false}
                issueKey="CORE-1"
                item={undefined}
                onShowDiff={() => {}}
                onToolbarChange={() => {}}
                onVersionChange={() => {}}
                showDiff={false}
              />
            </NoTooltips>
          </MarginProvider>
        </DocumentRuntime.Provider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

// ---- The way back, on the page's own stage

/** A document of no role beside the issue's spec and decisions document. */
const NOTES = issueArtifact("artifact-notes", "notes-md");
const ISSUE: IssueDetails = {
  ...issueDetails(),
  artifacts: [...(issueDetails().artifacts ?? []), NOTES],
};

/** Documents' list's own address, and the notes' own. */
const DOCUMENTS = buildIssuePath({ key: KEY, kind: "issue-documents" });
const NOTES_ADDRESS = buildIssuePath({ key: KEY, kind: "artifact", slug: NOTES.slug });

/** The chip that names the notes in a row's words (`RowText.tsx`). */
const NOTES_CHIP = `a[href="${NOTES_ADDRESS}"]`;

/** Where the router is, how it got there, and Back. */
function Where(): ReactNode {
  const { pathname, search } = useLocation();
  const navigate = useNavigate();
  return (
    <>
      <output data-move={useNavigationType()} data-testid="where">{`${pathname}${search}`}</output>
      <button onClick={() => navigate(-1)} type="button">
        Back
      </button>
    </>
  );
}

/** The view row, which binds the page's keys, over the stage at the address the router holds,
 *  under the page's own providers, inside the page's root, as the page's root draws them. */
function Page(): ReactNode {
  const { pathname, search } = useLocation();
  const route = parseIssuePath(pathname, search);
  if (route === undefined) throw new Error(`${pathname} names no route`);
  return (
    <div data-issue-page="">
      <ProvidersSlot issue={ISSUE} route={route}>
        <ViewRowSlot issue={ISSUE} route={route} />
        <StageSlot issue={ISSUE} route={route} />
        <Where />
      </ProvidersSlot>
    </div>
  );
}

/** The window as happy-dom draws it, whose width every media query of the page is answered
 *  from. */
const testWindow = window as unknown as {
  happyDOM: { setViewport(size: { width: number }): void };
  innerWidth: number;
};

/** The page at `address` at 1440 px, the issue's comments read as `comments`; `undraw` takes it
 *  away and puts back what it stubbed. */
function drawPage(
  address: string,
  comments = TWO_THREADS
): { readonly undraw: () => void; readonly view: RenderResult } {
  const width = testWindow.innerWidth;
  testWindow.happyDOM.setViewport({ width: DESKTOP_WIDTH });
  const reads = [...stubReads(), spyOn(api, "listComments").mockResolvedValue([...comments])];
  answerRequests(
    artifactRecordHandler((id) => (ISSUE.artifacts ?? []).find((one) => one.id === id))
  );
  const client = createQueryClient();
  const view = render(
    <MemoryRouter initialEntries={[address]}>
      <AppProviders client={client}>
        <DocumentRuntime.Provider value={fakeDocumentRuntime({ text: "The notes" }).runtime}>
          <Page />
        </DocumentRuntime.Provider>
      </AppProviders>
    </MemoryRouter>
  );
  return {
    undraw: () => {
      view.unmount();
      client.clear();
      for (const read of reads) read.mockRestore();
      testWindow.happyDOM.setViewport({ width });
    },
    view,
  };
}

/** The address the router is at. */
function address(): string {
  return screen.getByTestId("where").textContent ?? "";
}

/** The link of Documents' list's row of `name`, once the list draws it. */
async function listLink(name: string): Promise<HTMLElement> {
  return waitFor(() => {
    const link = [
      ...document.querySelectorAll<HTMLElement>(
        `[${DOCUMENTS_LIST_ATTRIBUTE}] [${DOCUMENT_OPEN_ATTRIBUTE}]`
      ),
    ].find((one) => one.textContent === name);
    if (link === undefined) throw new Error(`Documents' list draws no row of ${name}`);
    return link;
  });
}

/** The notes opened in place of the list or the outline, with the focus on their own box, which
 *  holds it as no control of theirs does. */
async function focusTheNotes(): Promise<void> {
  const heading = await screen.findByRole("heading", { name: NOTES.name });
  const box = heading.closest<HTMLElement>('[tabindex="-1"]') ?? heading;
  await act(async () => box.focus());
}

test("Back to Documents returns to the list with the focus on the document's row", async () => {
  const user = userEvent.setup();
  const { undraw } = drawPage(DOCUMENTS);
  try {
    await user.click(await listLink(NOTES.name));
    await screen.findByRole("heading", { name: NOTES.name });
    expect(
      screen.queryByRole("button", { name: "Back to the conversation" }),
      "the opened document still draws today's way back"
    ).toBeNull();
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.back_to_documents }));
    await waitFor(() => expect(address()).toBe(DOCUMENTS));
    expect(screen.getByTestId("where")).toHaveAttribute("data-move", "PUSH");
    const row = await listLink(NOTES.name);
    await waitFor(() => expect(row, "the focus is not on the document's row").toHaveFocus());
    // A push, as a link's follow is: the browser's Back returns to the document.
    await user.click(screen.getByRole("button", { name: "Back" }));
    await waitFor(() => expect(address()).toBe(NOTES_ADDRESS));
    expect(await screen.findByRole("heading", { name: NOTES.name })).toBeInTheDocument();
  } finally {
    undraw();
  }
});

test("Escape from a document the list opened returns to the list", async () => {
  const user = userEvent.setup();
  const { undraw } = drawPage(DOCUMENTS);
  try {
    await user.click(await listLink(NOTES.name));
    await focusTheNotes();
    await user.keyboard("{Escape}");
    await waitFor(() => expect(address()).toBe(DOCUMENTS));
    expect(screen.getByTestId("where")).toHaveAttribute("data-move", "PUSH");
    expect(screen.queryByRole("heading", { name: NOTES.name })).toBeNull();
    const row = await listLink(NOTES.name);
    await waitFor(() => expect(row, "the focus is not on the document's row").toHaveFocus());
  } finally {
    undraw();
  }
});

test("Escape from a document a chip opened returns to the chip's row with the focus on the chip", async () => {
  const user = userEvent.setup();
  // The second thread a head with no reply, which cites the notes: a row that is no folded
  // thread draws its words whole, the notes as a chip.
  const threads = TWO_THREADS.flatMap((comment) =>
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
  const { undraw, view } = drawPage(`/issues/${KEY}/conversation`, threads);
  const chipRow = `[data-issue-body] ${nodeRowSelectorOf("head-b")}`;
  const chipIn = () =>
    waitFor(() => {
      const chip = view.container.querySelector<HTMLElement>(`${chipRow} ${NOTES_CHIP}`);
      if (chip === null) throw new Error("the second thread's head draws no chip of the notes");
      return chip;
    });
  try {
    await user.click(await chipIn());
    await waitFor(() => expect(address()).toBe(NOTES_ADDRESS));
    await focusTheNotes();
    await user.keyboard("{Escape}");
    await waitFor(() => expect(address()).toBe(`/issues/${KEY}/conversation`));
    const chip = await chipIn();
    await waitFor(() => expect(chip, "the focus is not on the chip").toHaveFocus());
  } finally {
    undraw();
  }
});

// ---- What the opened document draws under its editor

/** Version `number` of a document, named `name` where one is given. */
function version(number: number, name?: string): Version {
  return {
    authors: [{ id: "alice", kind: "user" }],
    created_at: `2026-11-${String(number).padStart(2, "0")}T00:00:00Z`,
    mime: "text/markdown",
    named: name !== undefined,
    number,
    sha256: `sha-${number}`,
    size: 10,
    summary: name ?? null,
  };
}

test("an opened document draws every version 20 at a time and no Show all versions", async () => {
  const user = userEvent.setup();
  // 25 versions, two of them named.
  const versions = Array.from({ length: 25 }, (_, index) =>
    version(index + 1, index === 2 ? "Third draft" : index === 22 ? "Review copy" : undefined)
  );
  const view = renderOpened({ ...artifact, versions });
  try {
    const list = screen.getByRole("region", { name: `Versions for ${artifact.name}` });
    const downloads = () =>
      within(list)
        .getAllByRole("link", { name: /^Download version \d+$/ })
        .map((link) => link.textContent);
    // Newest first, named or not.
    expect(downloads()).toEqual(
      Array.from({ length: 20 }, (_, index) => `${CONTROL_LABELS.download_version} ${25 - index}`)
    );
    expect(list).toHaveTextContent("Review copy");
    expect(screen.queryByRole("button", { name: "Show all versions" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Show named versions" })).toBeNull();
    const more = within(list).getByRole("button", { name: SHOW_MORE });
    expect(more.textContent).toBe(showMoreName(5));
    await user.click(more);
    expect(downloads()).toHaveLength(25);
    expect(list).toHaveTextContent("Third draft");
    await waitFor(() =>
      expect(within(list).getByRole("link", { name: "Download version 5" })).toHaveFocus()
    );
    expect(within(list).queryByRole("button", { name: SHOW_MORE })).toBeNull();
  } finally {
    view.unmount();
  }
});

/** A comment of CORE-2 that cites the spec, the `index`th. */
function citingComment(index: number): GraphEdge {
  const id = `00000000-0000-4000-8000-${String(index).padStart(12, "0")}`;
  return {
    created_at: "2026-11-16T10:00:00Z",
    direction: "in",
    excerpt: { text: `CORE-2's comment ${index} cites the spec.` },
    kind: "mentions",
    node: graphNode({
      id,
      issue_key: "CORE-2",
      kind: "comment",
      project: "CORE",
      ref: `dispatch://CORE-2/comment/${id}`,
    }),
    source_seq: index,
  };
}

test("what an opened document references is drawn open, 20 at a time", async () => {
  const user = userEvent.setup();
  answerRequests(
    http.get("/api/v1/issues/:key/references", () =>
      Response.json({
        linked_from: [],
        links: [],
        members: Array.from({ length: 25 }, (_, index) => ({
          artifact: {
            ...artifact,
            id: `artifact-note-${index + 1}`,
            name: `Note ${index + 1}`,
            primary: false,
            slug: `note-${index + 1}`,
          },
          depth: 1,
          via: { id: "comment-1", kind: "comment" },
        })),
        truncated: false,
      } satisfies IssueReferences)
    ),
    http.get("/api/v1/references", ({ request }) =>
      Response.json({
        edges: Array.from({ length: 25 }, (_, index) => citingComment(index + 1)),
        node: graphNode({
          id: new URL(request.url).searchParams.get("to") ?? "",
          kind: "artifact",
        }),
      } satisfies GraphReferences)
    )
  );
  const view = renderOpened();
  try {
    const opened = screen.getByTestId("opened-artifact");
    // What it references: the issue's reference closure, open, its first 20 members.
    const references = await within(opened).findByRole("region", { name: "References" });
    expect(within(references).getAllByRole("link", { name: /^Note \d+$/ })).toHaveLength(20);
    expect(within(references).getByRole("button", { name: SHOW_MORE }).textContent).toBe(
      showMoreName(5)
    );
    // What references it: open, with no control to press first, its first 20 rows.
    expect(within(opened).queryByRole("button", { name: /^Referenced by/ })).toBeNull();
    const citing = within(opened).getByRole("region", { name: "Referenced by" });
    await waitFor(() =>
      expect(
        within(citing).getAllByRole("link", { name: NODE_REFERENCES_WORDS.kinds.comment })
      ).toHaveLength(20)
    );
    const more = within(citing).getByRole("button", { name: SHOW_MORE });
    expect(more.textContent).toBe(showMoreName(5));
    await user.click(more);
    expect(
      within(citing).getAllByRole("link", { name: NODE_REFERENCES_WORDS.kinds.comment })
    ).toHaveLength(25);
  } finally {
    view.unmount();
  }
});

// R16 (decision 259): a document whose stored history cannot load offers a Rebuild. Pressed by
// keyboard, the rebuild lands, the rebuilt document reads, and the offer leaves the page with the
// button that held the focus, which hands it to nothing (decision 223). The opened document's
// region then puts the focus on its heading, the document's name, and never leaves it on the
// page's body.
test("a Rebuild that succeeds puts the focus on the opened document's heading", async () => {
  const user = userEvent.setup();
  const getArtifactText = spyOn(api, "getArtifactText").mockRejectedValue(
    new ApiError(409, {
      code: "DOCUMENT_UNLOADABLE",
      error: "The stored history cannot load.",
    })
  );
  const rebuildArtifact = spyOn(api, "rebuildArtifact").mockResolvedValue({
    head: 1,
    removed_checkpoints: 0,
    removed_snapshots: 0,
    removed_updates: 3,
    source_version: 1,
    validation_error: "decode live document: unexpected end of input",
  });
  const confirm = spyOn(window, "confirm").mockReturnValue(true);
  const view = renderOpened();
  try {
    const rebuild = await screen.findByRole("button", { name: "Rebuild from the latest version" });
    const heading = screen.getByRole("heading", { level: 2, name: artifact.name });
    getArtifactText.mockResolvedValue({
      markdown: "The live document",
      token: "sha256:live-document",
      version: 1,
    });
    rebuild.focus();
    await user.keyboard("{Enter}");
    await waitFor(() => expect(rebuildArtifact).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(rebuild.isConnected).toBe(false));
    await waitFor(() => expect(heading).toHaveFocus());
    expect(
      screen.queryByRole("button", { name: "Rebuild from the latest version" })
    ).not.toBeInTheDocument();
    expect(confirm).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    confirm.mockRestore();
    rebuildArtifact.mockRestore();
    getArtifactText.mockRestore();
  }
});
