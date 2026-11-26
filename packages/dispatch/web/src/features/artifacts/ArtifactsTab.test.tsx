import { beforeAll, expect, jest, spyOn, test } from "bun:test";
import { onlineManager, QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: the dropped file below is a drop, which user-event has no form for
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useLocation } from "react-router-dom";

import { displayTree } from "../../__tests__/display-trees";
import { realClock, tick } from "../../__tests__/fake-clock";
import { graphNode } from "../../__tests__/graph-fixture";
import { heldAnswers } from "../../__tests__/held-answers";
import { heldUntilLate, pressesOf } from "../../__tests__/held-until-late";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError, REQUEST_DEADLINE_MS } from "../../api/client";
import { DispatchQueryClient } from "../../api/query-client";
import type {
  Artifact,
  ArtifactUploadResponse,
  DisplayTree,
  IssueReferences,
  IssueReferencesAnswer,
  ReferenceLink,
} from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { buildIssuePath } from "../refs/routes";
import { ArtifactsTab } from "./ArtifactsTab";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

const artifact = {
  created_at: "2026-09-09T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-1",
  issue_key: "CORE-1",
  project: "CORE",
  kind: "image",
  name: "diagram.png",
  primary: false,
  ref_key: "CORE-1/diagram-png",
  session_id: null,
  slug: "diagram-png",
  versions: [
    {
      authors: [{ id: "alice", kind: "user" }],
      created_at: "2026-09-09T00:00:00Z",
      mime: "image/png",
      named: false,
      number: 1,
      sha256: "first-sha",
      size: 10,
      summary: null,
    },
    {
      authors: [{ id: "alice", kind: "user" }],
      created_at: "2026-09-09T01:00:00Z",
      mime: "image/png",
      named: false,
      number: 2,
      sha256: "second-sha",
      size: 20,
      summary: null,
    },
  ],
  // `satisfies`, not an annotation: the tests below slice `artifact.versions`, which the route
  // table's `Version[] | null` would not let them.
} satisfies Artifact;

const noReferences: IssueReferences = {
  linked_from: [],
  links: [],
  members: [],
  truncated: false,
};

/** The tab's own address: its path, and its query, which is where its filter lives. */
function Location() {
  const { pathname, search } = useLocation();
  return (
    <>
      <output data-testid="location">{pathname}</output>
      <output data-testid="location-search">{search}</output>
    </>
  );
}

function renderTab(
  artifacts: Artifact[],
  references: IssueReferencesAnswer | Error = noReferences,
  {
    isClosed = false,
    queryClient = new QueryClient({
      defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
    }),
    search = "",
  }: {
    readonly isClosed?: boolean;
    /** The query client the tab reads through, a plain one unless the case needs the app's own. */
    readonly queryClient?: QueryClient;
    /** The query the tab is opened at, as a reload or a shared link hands it over. */
    readonly search?: string;
  } = {}
) {
  const listArtifacts = spyOn(api, "listArtifacts").mockResolvedValue(artifacts);
  const getIssueReferences = spyOn(api, "getIssueReferences");
  if (references instanceof Error) {
    getIssueReferences.mockRejectedValue(references);
  } else {
    getIssueReferences.mockResolvedValue(references);
  }
  const view = render(
    <MemoryRouter
      initialEntries={[`${buildIssuePath({ key: "CORE-1", kind: "artifacts" })}${search}`]}
    >
      <QueryClientProvider client={queryClient}>
        <ArtifactsTab isClosed={isClosed} />
        <Location />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { getIssueReferences, listArtifacts, view };
}

test("Create and Cancel let go at the document write deadline without making a second document", async () => {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const { getIssueReferences, listArtifacts, view } = renderTab([artifact], noReferences, {
    queryClient,
  });
  const upload = spyOn(api, "uploadArtifact").mockRejectedValue(
    new NetworkError("/api/v1/artifacts", new TypeError("Failed to fetch"))
  );
  try {
    const prepare = userEvent.setup();
    await prepare.click(await screen.findByRole("button", { name: "New document" }));
    await prepare.paste("Rollout");
    const button = screen.getByRole<HTMLButtonElement>("button", { name: "Create" });
    const cancel = screen.getByRole<HTMLButtonElement>("button", { name: "Cancel" });
    jest.useFakeTimers();
    const user = userOnFakeClock();
    const sent = () => pressesOf([upload]);
    await heldUntilLate(user, button, {
      alongside: (held) => {
        expect(cancel.getAttribute("aria-disabled"), "Cancel").toBe(held ? "true" : null);
      },
      label: "New document Rollout",
      name: "Create",
      queryClient,
      sent,
    });
    await user.keyboard("{Enter}");
    await tick(1_000);
    expect(sent()).toBe(1);
    const stillSending = /Create sent nothing:.*still being sent/;
    expect(screen.getByText(stillSending)).toBeInTheDocument();
    upload.mockRejectedValue(new ApiError(403, { error: "Document limits reached" }));
    await tick(30_000);
    expect(screen.queryByText(stillSending)).toBeNull();
    expect(screen.getByRole("alert")).toHaveTextContent("Document limits reached");
    await user.keyboard("{Enter}");
    await tick(1_000);
    expect(sent()).toBe(2);
  } finally {
    realClock();
    view.unmount();
    queryClient.clear();
    upload.mockRestore();
    listArtifacts.mockRestore();
    getIssueReferences.mockRestore();
  }
});

test("renders one row per artifact with no compare controls", async () => {
  const notes: Artifact = {
    ...artifact,
    id: "artifact-notes",
    kind: "doc",
    name: "notes.md",
    slug: "notes-md",
    versions: artifact.versions.slice(0, 1),
  };
  const { getIssueReferences, listArtifacts, view } = renderTab([artifact, notes]);

  try {
    await screen.findByTestId("artifact-diagram-png");
    expect(screen.getByTestId("artifact-notes-md")).not.toBeNull();
    expect(screen.queryByLabelText(/Compare .* from/)).not.toBeInTheDocument();
    expect(screen.queryByLabelText(/Compare .* to/)).not.toBeInTheDocument();
    expect(screen.queryByText("Compare versions")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Show all versions" })).not.toBeInTheDocument();
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    getIssueReferences.mockRestore();
  }
});

// W41. A closed issue takes neither a new document nor an upload, and Dispatch would refuse an
// upload only after its whole body went up: the tab refuses both before a file is picked, with
// the reason on each control - and on the one line beside them, for a reader with no pointer -
// and a file dropped on it is not staged. W46. The hidden file input is disabled too, so no
// keyboard path opens a chooser whose file is ignored.
test("a closed issue's Artifacts tab refuses a new document and an upload before a file is picked", async () => {
  const user = userEvent.setup();
  const { getIssueReferences, listArtifacts, view } = renderTab([artifact], noReferences, {
    isClosed: true,
  });
  // Spied on HTMLElement, the prototype that owns the method: spying on HTMLInputElement would
  // leave an own property behind that shadows every later spy on HTMLElement.
  const pick = spyOn(HTMLElement.prototype, "click");
  try {
    await screen.findByTestId("artifact-diagram-png");
    const upload = screen.getByRole("button", { name: "Upload" });
    const create = screen.getByRole("button", { name: "New document" });
    const refusal = "This issue is closed. Reopen it to add a document.";
    expect(upload).toHaveAttribute("aria-disabled", "true");
    expect(upload).toHaveAttribute("title", refusal);
    expect(create).toHaveAttribute("aria-disabled", "true");
    expect(create).toHaveAttribute("title", refusal);
    const line = screen.getByText(refusal);
    expect(upload.getAttribute("aria-describedby")).toBe(line.id);
    expect(create.getAttribute("aria-describedby")).toBe(line.id);
    await user.click(create);
    expect(screen.queryByRole("form", { name: "New document" })).not.toBeInTheDocument();
    await user.click(upload);
    expect(pick).not.toHaveBeenCalled();
    const file = new File(["png"], "late.png", { type: "image/png" });
    // A drop of files, which user-event has no form for.
    fireEvent.drop(screen.getByTestId("artifact-diagram-png"), { dataTransfer: { files: [file] } });
    expect(screen.queryByText("late.png")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Summary (optional)")).not.toBeInTheDocument();
    const input = screen.getByLabelText("Upload artifact");
    expect(input instanceof HTMLInputElement && input.disabled).toBe(true);
  } finally {
    pick.mockRestore();
    view.unmount();
    listArtifacts.mockRestore();
    getIssueReferences.mockRestore();
  }
});

const madeDocument = {
  ...artifact,
  id: "artifact-design-notes",
  kind: "doc",
  name: "Design notes",
  ref_key: "CORE-1/design-notes",
  slug: "design-notes",
  versions: artifact.versions.slice(0, 1),
} satisfies Artifact;

function madeResponse() {
  const [version] = madeDocument.versions;
  if (version === undefined) {
    throw new Error("the made document fixture requires a version");
  }
  return { artifact: madeDocument, version };
}

// The control sends the title alone, so the new document holds nothing, and the person is taken
// to it on the page they are already on: the issue's artifact route renders the editor.
test("New document on an issue makes an empty document and opens it on the same page", async () => {
  const user = userEvent.setup();
  const { getIssueReferences, listArtifacts, view } = renderTab([]);
  const uploadArtifact = spyOn(api, "uploadArtifact").mockResolvedValue(madeResponse());

  try {
    await screen.findByText("No artifacts yet.");
    await user.click(screen.getByRole("button", { name: "New document" }));
    await user.click(screen.getByRole("textbox", { name: "Title" }));
    await user.paste("Design notes");
    await user.click(screen.getByRole("button", { name: "Create" }));

    await waitFor(() =>
      expect(uploadArtifact).toHaveBeenCalledWith(
        { issue: "CORE-1" },
        { name: "Design notes" },
        keyedAttempt
      )
    );
    expect(screen.getByTestId("location").textContent).toBe(
      "/issues/CORE-1/artifacts/design-notes"
    );
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    getIssueReferences.mockRestore();
    uploadArtifact.mockRestore();
  }
});

// The write compares the title with each of the owner's names and slugs (`$2 in (name, slug)`),
// so the control refuses a slug it lists too, and names the artifact that holds the title for
// what it is: the list holds the issue's images and files beside its documents.
test("New document on an issue refuses a title the issue already holds", async () => {
  const user = userEvent.setup();
  const { getIssueReferences, listArtifacts, view } = renderTab([artifact]);
  const uploadArtifact = spyOn(api, "uploadArtifact").mockResolvedValue(madeResponse());

  try {
    await screen.findByTestId("artifact-diagram-png");
    await user.click(screen.getByRole("button", { name: "New document" }));
    await user.click(screen.getByRole("textbox", { name: "Title" }));
    await user.paste("diagram-png");
    await user.click(screen.getByRole("button", { name: "Create" }));

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "An artifact on this issue already holds that title: diagram.png."
    );
    expect(uploadArtifact).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    getIssueReferences.mockRestore();
    uploadArtifact.mockRestore();
  }
});

// INFRA-3135. The focus follows the form: New document opens it with the focus in Title, so the
// title is typed at once, and Cancel closes it with the focus back on New document, the control
// that opened it, rather than dropped on the page.
test("New document puts the focus in Title, and Cancel gives it back to New document", async () => {
  const user = userEvent.setup();
  const { getIssueReferences, listArtifacts, view } = renderTab([]);

  try {
    await screen.findByText("No artifacts yet.");
    await user.click(screen.getByRole("button", { name: "New document" }));
    expect(screen.getByRole("textbox", { name: "Title" })).toHaveFocus();

    await user.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.getByRole("button", { name: "New document" })).toHaveFocus();
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    getIssueReferences.mockRestore();
  }
});

// INFRA-3135. The same by the keyboard alone: Tab reaches the filter box and then New document,
// and Enter opens the form with the focus in Title; Tab twice reaches Cancel, and Enter closes
// the form with the focus back on New document, so the next Tab goes on to Upload rather than
// starting again from the top of the page.
test("New document by the keyboard: Enter puts the focus in Title, and Cancel gives it back", async () => {
  const user = userEvent.setup();
  const { getIssueReferences, listArtifacts, view } = renderTab([]);

  try {
    await screen.findByText("No artifacts yet.");
    await user.tab();
    await user.tab();
    await user.keyboard("{Enter}");
    expect(screen.getByRole("textbox", { name: "Title" })).toHaveFocus();

    await user.tab();
    await user.tab();
    await user.keyboard("{Enter}");
    expect(screen.getByRole("button", { name: "New document" })).toHaveFocus();
    await user.tab();
    expect(screen.getByRole("button", { name: "Upload" })).toHaveFocus();
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    getIssueReferences.mockRestore();
  }
});

// A close the reader has moved away from is not the form's to undo. While Create is out they go
// to the filter box; the write answers, the form closes and the document opens, and the focus
// stays in the filter box rather than going back to New document.
test("New document leaves the focus where the reader moved it while Create was out", async () => {
  const user = userEvent.setup();
  const { getIssueReferences, listArtifacts, view } = renderTab([]);
  const answer = Promise.withResolvers<ArtifactUploadResponse>();
  const uploadArtifact = spyOn(api, "uploadArtifact").mockReturnValue(answer.promise);

  try {
    await screen.findByText("No artifacts yet.");
    await user.click(screen.getByRole("button", { name: "New document" }));
    // The title goes where the opening put the focus.
    expect(screen.getByRole("textbox", { name: "Title" })).toHaveFocus();
    await user.paste("Design notes");
    await user.click(screen.getByRole("button", { name: "Create" }));
    await waitFor(() => expect(uploadArtifact).toHaveBeenCalled());
    const filter = screen.getByRole("searchbox", { name: "Filter artifacts" });
    await user.click(filter);

    answer.resolve(madeResponse());
    await waitFor(() =>
      expect(screen.getByTestId("location").textContent).toBe(
        "/issues/CORE-1/artifacts/design-notes"
      )
    );
    expect(screen.queryByRole("form", { name: "New document" })).not.toBeInTheDocument();
    expect(filter).toHaveFocus();
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    getIssueReferences.mockRestore();
    uploadArtifact.mockRestore();
  }
});

// A row names how big its latest version is stored, a document's markdown as a file's bytes, so a
// person finds a document near the size cap from the list without opening it.
test("a row names its latest version's stored size", async () => {
  const spec: Artifact = {
    ...artifact,
    id: "artifact-spec",
    kind: "doc",
    name: "near-cap.md",
    slug: "near-cap-md",
    versions: [
      {
        authors: [{ id: "alice", kind: "user" }],
        created_at: "2026-09-09T02:00:00Z",
        named: false,
        number: 1,
        size: 1_048_475,
        summary: null,
      },
    ],
  };
  const { getIssueReferences, listArtifacts, view } = renderTab([artifact, spec]);

  try {
    const document = await screen.findByTestId("artifact-near-cap-md");
    expect(document.querySelector("p")?.textContent).toContain("doc · 1 version · 1023.9 KB ·");
    expect(screen.getByTestId("artifact-diagram-png").querySelector("p")?.textContent).toContain(
      "image · 2 versions · 20 B ·"
    );
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    getIssueReferences.mockRestore();
  }
});

test("artifacts tab renders no make-primary control", async () => {
  const artifacts: Artifact[] = [
    {
      ...artifact,
      id: "artifact-spec",
      kind: "doc",
      name: "spec.md",
      primary: true,
      slug: "spec",
      versions: artifact.versions.slice(0, 1),
    },
    {
      ...artifact,
      id: "artifact-notes",
      kind: "doc",
      name: "notes.md",
      primary: false,
      slug: "notes-md",
      versions: artifact.versions.slice(0, 1),
    },
    artifact,
  ];
  const { getIssueReferences, listArtifacts, view } = renderTab(artifacts);

  try {
    await screen.findByTestId("artifact-spec");
    expect(screen.getByTestId("artifact-spec")).toHaveTextContent("Primary");
    expect(screen.queryByRole("button", { name: /make primary/i })).not.toBeInTheDocument();
    expect(screen.queryByText(/Not\s+primary/)).not.toBeInTheDocument();
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    getIssueReferences.mockRestore();
  }
});

test("a primary artifact sorts first regardless of upload order", async () => {
  const older: Artifact = {
    ...artifact,
    id: "artifact-older",
    name: "older.png",
    slug: "older-png",
  };
  const primary: Artifact = {
    ...artifact,
    id: "artifact-primary",
    name: "primary.png",
    primary: true,
    slug: "primary-png",
  };
  const { getIssueReferences, listArtifacts, view } = renderTab([older, primary]);

  try {
    await screen.findByTestId("artifact-older-png");
    const rows = screen.getAllByRole("listitem");
    expect(rows[0]?.getAttribute("data-testid")).toBe("artifact-primary-png");
    expect(rows[1]?.getAttribute("data-testid")).toBe("artifact-older-png");
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    getIssueReferences.mockRestore();
  }
});

test("artifacts saved in the same second list newest first by time, not by the timestamp strings", async () => {
  // As text `…00.12Z` sorts after `…00.123456Z`, the later time.
  const savedAt = (id: string, createdAt: string): Artifact => ({
    ...artifact,
    id: `artifact-${id}`,
    name: `${id}.png`,
    slug: `${id}-png`,
    versions: artifact.versions
      .slice(0, 1)
      .map((version) => ({ ...version, created_at: createdAt })),
  });
  const { getIssueReferences, listArtifacts, view } = renderTab([
    savedAt("earlier", "2026-09-09T01:00:00.12Z"),
    savedAt("later", "2026-09-09T01:00:00.123456Z"),
  ]);

  try {
    await screen.findByTestId("artifact-later-png");
    expect(screen.getAllByRole("listitem").map((row) => row.getAttribute("data-testid"))).toEqual([
      "artifact-later-png",
      "artifact-earlier-png",
    ]);
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    getIssueReferences.mockRestore();
  }
});

test("the filter input narrows rows by name and kind", async () => {
  const user = userEvent.setup();
  const notes: Artifact = {
    ...artifact,
    id: "artifact-notes",
    kind: "doc",
    name: "notes.md",
    slug: "notes-md",
  };
  const { getIssueReferences, listArtifacts, view } = renderTab([artifact, notes]);

  try {
    await screen.findByTestId("artifact-diagram-png");
    const filter = screen.getByLabelText("Filter artifacts");
    await user.click(filter);
    await user.paste("notes");
    expect(screen.queryByTestId("artifact-diagram-png")).not.toBeInTheDocument();
    expect(screen.getByTestId("artifact-notes-md")).not.toBeNull();

    await user.clear(filter);
    await user.click(filter);
    await user.paste("image");
    expect(screen.getByTestId("artifact-diagram-png")).not.toBeNull();
    expect(screen.queryByTestId("artifact-notes-md")).not.toBeInTheDocument();
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    getIssueReferences.mockRestore();
  }
});

// The tab's filter is part of the address, so a reader who reloads, shares the tab or walks
// back to it finds the same narrowed list rather than the whole one.
test("the filter rides in the address: absent until set, and found again after a reload", async () => {
  const user = userEvent.setup();
  const notes: Artifact = {
    ...artifact,
    id: "artifact-notes",
    kind: "doc",
    name: "notes.md",
    slug: "notes-md",
  };
  const typed = renderTab([artifact, notes]);

  try {
    await screen.findByTestId("artifact-diagram-png");
    // Nothing the reader has not asked for: the address holds no `q` until one is typed.
    expect(screen.getByTestId("location-search").textContent).toBe("");

    const filter = screen.getByRole("searchbox", { name: "Filter artifacts" });
    await user.click(filter);
    await user.paste("notes");
    await waitFor(() => expect(screen.getByTestId("location-search").textContent).toBe("?q=notes"));
    expect(screen.queryByTestId("artifact-diagram-png")).not.toBeInTheDocument();

    await user.clear(filter);
    await waitFor(() => expect(screen.getByTestId("location-search").textContent).toBe(""));
  } finally {
    typed.view.unmount();
    typed.listArtifacts.mockRestore();
    typed.getIssueReferences.mockRestore();
  }

  // The same address opened afresh, as a reload hands it over: the box holds those words and
  // the list is already narrowed by them, before anybody types.
  const reloaded = renderTab([artifact, notes], noReferences, { search: "?q=notes" });

  try {
    await screen.findByTestId("artifact-notes-md");
    expect(screen.getByRole("searchbox", { name: "Filter artifacts" })).toHaveValue("notes");
    expect(screen.queryByTestId("artifact-diagram-png")).not.toBeInTheDocument();
  } finally {
    reloaded.view.unmount();
    reloaded.listArtifacts.mockRestore();
    reloaded.getIssueReferences.mockRestore();
  }
});

// An answer without `links` and `linked_from`, as a Dispatch from before INFRA-4704 gives: the tab
// draws its members as it always did, with their depth and the line for a walk cut at 8 hops.
test("From an answer without links, References lists closure members with chip, via, and depth, names truncation, and renders an artifact source without an issue key, collapsed by default", async () => {
  const referencedDocument: Artifact = {
    ...artifact,
    id: "artifact-notes",
    issue_key: null,
    kind: "doc",
    name: "Design notes",
    primary: false,
    slug: "design-notes",
  };
  const { getIssueReferences, listArtifacts, view } = renderTab([artifact], {
    members: [
      {
        artifact: referencedDocument,
        depth: 1,
        via: { id: "comment-1", kind: "comment" },
      },
    ],
    truncated: true,
  });

  try {
    const summary = await screen.findByText("References (1)");
    const details = summary.closest("details");
    expect(details).toBeInstanceOf(HTMLDetailsElement);
    expect((details as HTMLDetailsElement).open).toBe(false);

    const references = within(view.container).getByRole("region", { name: "References" });
    expect(within(references).getByRole("link", { name: "Design notes" })).toHaveAttribute(
      "href",
      "/projects/CORE/documents/design-notes"
    );
    expect(references).toHaveTextContent("CORE");
    expect(references).toHaveTextContent("via comment");
    expect(references).toHaveTextContent("depth 1");
    expect(references).toHaveTextContent("more references beyond 8 hops");
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    getIssueReferences.mockRestore();
  }
});

// dispatch://INFRA-4704: from an answer with `links`, References lists each node the issue's own
// content links to, one hop out, and Referenced by under it each node that links to the issue or
// to its own content, one hop in. A node of both is drawn once, in References, marked as linking
// back. A one-hop read reaches no 8th hop, so no row draws a depth and no line says the read was
// cut, though the answer says `truncated`. Each row's node and name come in the answer, so the tab
// reads nothing to draw them: its two reads, which the test answers above the network, are the
// only requests it makes, and no request reaches `fetch`.
test("References lists the issue's links and Referenced by the nodes that link to it, each node once, with no depth and no read beyond the references route", async () => {
  const comment = "cccccccc-0000-4000-8000-000000000009";
  const message = "eeeeeeee-0000-4000-8000-000000000007";
  const viaComment = { id: "comment-1", kind: "comment" };
  const projectDocument = graphNode({
    id: "artifact-a",
    kind: "artifact",
    project: "CORE",
    ref: "dispatch://CORE/artifact/a-md",
  });
  const issueDocument = graphNode({
    id: "artifact-c",
    issue_key: "CORE-7",
    kind: "artifact",
    project: "CORE",
    ref: "dispatch://CORE-7/artifact/c-md",
  });
  const answer: IssueReferences = {
    members: [
      {
        artifact: {
          ...artifact,
          id: "artifact-a",
          issue_key: null,
          kind: "doc",
          name: "a.md",
          ref_key: "CORE/a-md",
          slug: "a-md",
        },
        depth: 1,
        via: viaComment,
      },
      {
        artifact: {
          ...artifact,
          id: "artifact-c",
          issue_key: "CORE-7",
          kind: "doc",
          name: "c.md",
          ref_key: "CORE-7/c-md",
          slug: "c-md",
        },
        depth: 1,
        via: viaComment,
      },
    ],
    truncated: true,
    links: [
      { kinds: ["mentions"], name: "a.md", node: projectDocument, via: viaComment },
      { kinds: ["mentions"], name: "c.md", node: issueDocument, via: viaComment },
      {
        kinds: ["mentions"],
        name: "Ship the parser",
        node: graphNode({
          id: "CORE-9",
          issue_key: "CORE-9",
          kind: "issue",
          project: "CORE",
          ref: "dispatch://CORE-9",
        }),
        via: { id: "artifact-1", kind: "artifact" },
      },
      {
        kinds: ["mentions"],
        name: "The parser lands on Friday.",
        node: graphNode({
          id: comment,
          issue_key: "CORE-9",
          kind: "comment",
          project: "CORE",
          ref: `dispatch://CORE-9/comment/${comment}`,
        }),
        via: viaComment,
      },
    ],
    linked_from: [
      {
        kinds: ["mentions"],
        name: "c.md",
        node: issueDocument,
        via: { id: "CORE-1", kind: "issue" },
      },
      {
        kinds: ["mentions"],
        name: "e.md",
        node: graphNode({
          id: "artifact-e",
          kind: "artifact",
          project: "OPS",
          ref: "dispatch://OPS/artifact/e-md",
        }),
        via: { id: "CORE-1/diagram-png", kind: "artifact" },
      },
      {
        kinds: ["mentions"],
        name: "See the diagram",
        node: graphNode({
          id: message,
          issue_key: "CORE-7",
          kind: "message",
          project: "CORE",
          ref: `dispatch://CORE-7/message/${message}`,
        }),
        via: { id: "CORE-1", kind: "issue" },
      },
    ],
  };
  const fetches = spyOn(globalThis, "fetch");
  const { getIssueReferences, listArtifacts, view } = renderTab([artifact], answer);

  try {
    await screen.findByText("References (4)");
    expect(screen.getByText("Referenced by (2)")).toBeInTheDocument();
    const references = within(view.container).getByRole("region", {
      hidden: true,
      name: "References",
    });
    const referencedBy = within(view.container).getByRole("region", {
      hidden: true,
      name: "Referenced by",
    });
    // Each list's rows, Referenced by's left out of References' should it be drawn inside it.
    const referencedByRows = within(referencedBy).getAllByRole("listitem", { hidden: true });
    const referenceRows = within(references)
      .getAllByRole("listitem", { hidden: true })
      .filter((row) => !referencedBy.contains(row));
    // The name and the address of the link a row opens.
    const linkOf = (row: HTMLElement) => {
      const link = within(row).getAllByRole("link", { hidden: true })[0];
      return [link?.textContent, link?.getAttribute("href")];
    };

    expect(referenceRows.map(linkOf)).toEqual([
      ["a.md", "/projects/CORE/documents/a-md"],
      ["c.md", "/issues/CORE-7/artifacts/c-md"],
      ["Ship the parser", "/issues/CORE-9"],
      ["The parser lands on Friday.", `/issues/CORE-9/comments/${comment}`],
    ]);
    const [projectRow, bothRow, issueRow, commentRow] = referenceRows;
    // A document's row names its owner, as a member's does.
    expect(projectRow).toHaveTextContent("CORE");
    expect(projectRow).toHaveTextContent("artifact · via comment");
    expect(bothRow).toHaveTextContent("CORE-7");
    expect(bothRow).toHaveTextContent("artifact · via comment");
    expect(bothRow).toHaveTextContent("also references this issue");
    expect(issueRow).toHaveTextContent("issue · via artifact");
    expect(commentRow).toHaveTextContent("comment · via comment");
    for (const row of [projectRow, issueRow, commentRow]) {
      expect(row).not.toHaveTextContent("also references this issue");
    }

    // c.md links to the issue too, and is drawn once, in References.
    expect(referencedByRows.map(linkOf)).toEqual([
      ["e.md", "/projects/OPS/documents/e-md"],
      ["See the diagram", `/issues/CORE-7/messages/${message}`],
    ]);
    const [documentRow, messageRow] = referencedByRows;
    expect(documentRow).toHaveTextContent("OPS");
    expect(documentRow).toHaveTextContent("artifact · to artifact");
    expect(messageRow).toHaveTextContent("message · to issue");
    expect(screen.getAllByRole("link", { hidden: true, name: "c.md" })).toHaveLength(1);

    expect(references).not.toHaveTextContent("depth");
    expect(referencedBy).not.toHaveTextContent("depth");
    expect(view.container).not.toHaveTextContent("more references beyond 8 hops");

    expect(getIssueReferences).toHaveBeenCalledTimes(1);
    expect(getIssueReferences).toHaveBeenCalledWith("CORE-1");
    expect(
      fetches.mock.calls.map(([input]) => (input instanceof Request ? input.url : String(input)))
    ).toEqual([]);
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    getIssueReferences.mockRestore();
    fetches.mockRestore();
  }
});

// An issue that links to nothing and that other nodes link to: `links` is empty and `linked_from`
// holds a row. The section is collapsed by default, so what it shows before anyone opens it is all
// a reader learns from a glance. A closed disclosure that reads "References (0)" over the only rows
// the read gave hides them: nothing on the closed section says that a node links to the issue.
test("an issue only other nodes link to shows Referenced by (1) before the section is opened", async () => {
  const comment = "dddddddd-0000-4000-8000-000000000003";
  const answer: IssueReferences = {
    links: [],
    linked_from: [
      {
        kinds: ["mentions"],
        name: "This links to CORE-1.",
        node: graphNode({
          id: comment,
          issue_key: "OPS-1",
          kind: "comment",
          project: "OPS",
          ref: `dispatch://OPS-1/comment/${comment}`,
        }),
        via: { id: "CORE-1", kind: "issue" },
      },
    ],
    members: [],
    truncated: false,
  };
  // The row's name is drawn as its plain-text projection, which reads the bare key in it, CORE-1,
  // by CORE-1's summary: the read is answered here, and the row waits for the name it gives, so no
  // read is left out when the row ends.
  const getIssueSummary = spyOn(api, "getIssueSummary").mockResolvedValue({
    documents: [],
    key: "CORE-1",
    project: "CORE",
    status: "in_progress",
    title: "Ship the parser",
  });
  const { getIssueReferences, listArtifacts, view } = renderTab([artifact], answer);

  try {
    // Every element that names the count, kept where a reader sees it with the section closed:
    // outside a closed <details>, or in that <details>' own <summary>.
    const shown = (await screen.findAllByText(/Referenced by \(1\)/)).filter((element) => {
      const closed = element.closest("details:not([open])");
      return closed === null || (closed.querySelector("summary")?.contains(element) ?? false);
    });
    expect(shown).not.toHaveLength(0);
    await screen.findByRole("link", {
      hidden: true,
      name: "This links to CORE-1 Ship the parser.",
    });
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    getIssueReferences.mockRestore();
    getIssueSummary.mockRestore();
  }
});

/** A comment of OPS-1 whose body holds a bare issue address, which the issue's own content links
 *  to: the one row of `links` in the rows below, named by that body. The rows that read an ask's
 *  name and a message's give an ask and a message of OPS-1 (`addressedAsk`, `addressedMessage`)
 *  the same text. */
const addressedComment = "ffffffff-0000-4000-8000-000000000005";
const addressedAsk = "ffffffff-0000-4000-8000-000000000006";
const addressedMessage = "ffffffff-0000-4000-8000-000000000008";
const addressedName = "Decided in dispatch://CORE-2 on **Friday**.";

/** An answer whose one row is OPS-1's `kind` `id`, `addressedComment` unless given, carrying
 *  `nameTree` as its `name_tree`, or none, as a Dispatch from before `name_tree` answers. */
function addressedAnswer(
  nameTree: DisplayTree | undefined,
  kind: "ask" | "comment" | "message" = "comment",
  id = addressedComment
): IssueReferences {
  return {
    links: [
      {
        kinds: ["mentions"],
        name: addressedName,
        name_tree: nameTree,
        node: graphNode({
          id,
          issue_key: "OPS-1",
          kind,
          project: "OPS",
          ref: `dispatch://OPS-1/${kind}/${id}`,
        }),
        via: { id: "comment-1", kind: "comment" },
      },
    ],
    linked_from: [],
    members: [],
    truncated: false,
  };
}

/** Draws `answer`'s tab with CORE-2's summary answering its read, and hands the row's link, found
 *  by the name the row should read, to `check`. */
async function withAddressedRow(
  answer: IssueReferences,
  check: (link: HTMLElement) => void
): Promise<void> {
  const getIssueSummary = spyOn(api, "getIssueSummary").mockResolvedValue({
    documents: [],
    key: "CORE-2",
    project: "CORE",
    status: "in_progress",
    title: "Cut the release",
  });
  const { getIssueReferences, listArtifacts, view } = renderTab([artifact], answer);
  try {
    check(
      await screen.findByRole("link", {
        hidden: true,
        name: "Decided in CORE-2 Cut the release on Friday.",
      })
    );
    expect(getIssueSummary).toHaveBeenCalledWith("CORE-2");
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    getIssueReferences.mockRestore();
    getIssueSummary.mockRestore();
  }
}

// The reading list's `artifacts-reference-name`: a row's name is the words of a link, which hold
// no markup, so the tab draws its plain-text projection from the tree the read sent of it. The
// bold goes, and the bare issue address reads as its target's name, CORE-2's key and title, which
// its own read answers. Drawn as the characters it is, the link would read
// `Decided in dispatch://CORE-2 on **Friday**.`.
test("a references row reads a reference in its name as its target's name", async () => {
  await withAddressedRow(addressedAnswer(displayTree(addressedName)), (link) => {
    expect(link).toHaveAttribute("href", `/issues/OPS-1/comments/${addressedComment}`);
    expect(link.textContent).not.toContain("dispatch://");
  });
});

// A Dispatch from before `name_tree` sends a row's name with no tree: the tab reads the name's
// parse on the page in its place, never the characters as written, so the address still reads as
// its target's name.
test("a references row with no name_tree reads its name's parse, a reference in it as its target's name", async () => {
  await withAddressedRow(addressedAnswer(undefined), (link) => {
    expect(link).toHaveAttribute("href", `/issues/OPS-1/comments/${addressedComment}`);
    expect(link.textContent).not.toContain("dispatch://");
  });
});

// An ask's name and a message's are the head of a record's text as a comment's is (`nameSource`),
// so each is drawn as its plain-text projection too: the bold goes, and the bare issue address
// reads as CORE-2's key and title.
test("a references row named by an ask's text reads a reference in it as its target's name", async () => {
  await withAddressedRow(
    addressedAnswer(displayTree(addressedName), "ask", addressedAsk),
    (link) => {
      expect(link).toHaveAttribute("href", `/issues/OPS-1/asks/${addressedAsk}`);
      expect(link.textContent).not.toContain("dispatch://");
    }
  );
});

test("a references row named by a message's text reads a reference in it as its target's name", async () => {
  await withAddressedRow(
    addressedAnswer(displayTree(addressedName), "message", addressedMessage),
    (link) => {
      expect(link).toHaveAttribute("href", `/issues/OPS-1/messages/${addressedMessage}`);
      expect(link.textContent).not.toContain("dispatch://");
    }
  );
});

// A row whose name is no record's text - a document's name, an issue's title - is drawn as the
// characters it is, as the reference graph's excerpts draw those: a document called
// `*draft*.md` keeps its asterisks, which a projection would read as emphasis and drop.
test("a references row named by a document's name draws it as the characters it is", async () => {
  const documentName = "*draft*.md";
  const { getIssueReferences, listArtifacts, view } = renderTab([artifact], {
    links: [
      {
        kinds: ["mentions"],
        name: documentName,
        name_tree: displayTree(documentName),
        node: graphNode({
          id: "ops-draft",
          kind: "artifact",
          project: "OPS",
          ref: "dispatch://OPS/artifact/draft-md",
        }),
        via: { id: "comment-1", kind: "comment" },
      },
    ],
    linked_from: [],
    members: [],
    truncated: false,
  });
  try {
    expect(await screen.findByRole("link", { hidden: true, name: documentName })).toBeDefined();
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    getIssueReferences.mockRestore();
  }
});

/** Renders the tab over a references answer whose list `list` holds `row` alone, with `drawnRow`,
 *  which answers that row's item once its disclosure is drawn. */
function renderReferenceRow(list: "links" | "linked_from", row: ReferenceLink) {
  const rendered = renderTab([artifact], {
    ...noReferences,
    linked_from: list === "linked_from" ? [row] : [],
    links: list === "links" ? [row] : [],
  });
  const label = list === "links" ? "References" : "Referenced by";
  const drawnRow = async () => {
    await screen.findByText(`${label} (1)`);
    const section = within(rendered.view.container).getByRole("region", {
      hidden: true,
      name: label,
    });
    return within(section).getByRole("listitem", { hidden: true });
  };
  return { ...rendered, drawnRow };
}

// INFRA-1523: a comment its author deleted keeps its row, and the row says what became of it.
// The read names a comment by the head of its body, which the delete cleared, so the deleted
// line (`DeletedBody`: who deleted it, and when) names the row in place of the read's words, and
// the row still opens the post's page. The fixture's `name` holds the post's former words, so a
// row that fell back to them would show them; the post was edited before it was deleted, so
// ` · edited` could be drawn, and only the row's reading of the delete keeps it off.
test("a References row for a comment its author deleted draws the deleted line and none of its words, and still links to the comment", async () => {
  const comment = "aaaaaaaa-0000-4000-8000-000000000011";
  const deletedAt = "2026-10-20T11:00:00Z";
  const { drawnRow, getIssueReferences, listArtifacts, view } = renderReferenceRow("links", {
    kinds: ["mentions"],
    name: "The parser lands on Friday.",
    node: graphNode({
      deleted_at: deletedAt,
      deleted_by: { id: "alice", kind: "user" },
      edited_at: "2026-10-20T10:00:00Z",
      id: comment,
      issue_key: "CORE-9",
      kind: "comment",
      project: "CORE",
      ref: `dispatch://CORE-9/comment/${comment}`,
    }),
    via: { id: "comment-1", kind: "comment" },
  });

  try {
    const row = await drawnRow();
    const link = within(row).getByRole("link", { hidden: true });
    expect(link).toHaveAttribute("href", `/issues/CORE-9/comments/${comment}`);
    const line = link.querySelector("[data-post-deleted]");
    expect(line?.textContent).toMatch(/^Deleted by alice · /);
    expect(line?.querySelector("time")).toHaveAttribute("datetime", deletedAt);
    expect(row).not.toHaveTextContent("The parser lands on Friday.");
    expect(row).not.toHaveTextContent("edited");
    expect(row).toHaveTextContent("comment · via comment");
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    getIssueReferences.mockRestore();
  }
});

// INFRA-1523: a message its author edited is named by the words the read gives for it, then
// ` · edited`, as its byline says it (`EditedMark`), inside the link that opens it.
test("a Referenced by row for a message its author edited names it, then says · edited", async () => {
  const message = "eeeeeeee-0000-4000-8000-000000000012";
  const { drawnRow, getIssueReferences, listArtifacts, view } = renderReferenceRow("linked_from", {
    kinds: ["mentions"],
    name: "See the diagram",
    node: graphNode({
      edited_at: "2026-10-20T10:30:00Z",
      id: message,
      issue_key: "CORE-7",
      kind: "message",
      project: "CORE",
      ref: `dispatch://CORE-7/message/${message}`,
    }),
    via: { id: "CORE-1", kind: "issue" },
  });

  try {
    const row = await drawnRow();
    const link = within(row).getByRole("link", { hidden: true });
    expect(link.textContent).toBe("See the diagram · edited");
    expect(link).toHaveAttribute("href", `/issues/CORE-7/messages/${message}`);
    expect(row.querySelector("[data-post-deleted]")).toBeNull();
    expect(row).toHaveTextContent("message · to issue");
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    getIssueReferences.mockRestore();
  }
});

// INFRA-1523: a block of a comment carries its comment's delete mark (`refs/item_block_nodes.go`,
// `itemBlockNodes`), so a row for a block of a deleted comment draws the deleted line too, and
// never the block's words the read gave.
test("a References row for a block of a deleted comment draws the deleted line and none of its words", async () => {
  const comment = "bbbbbbbb-0000-4000-8000-000000000013";
  const deletedAt = "2026-10-21T09:00:00Z";
  const { drawnRow, getIssueReferences, listArtifacts, view } = renderReferenceRow("links", {
    kinds: ["mentions"],
    name: "Ship it once the parser lands.",
    node: graphNode({
      block_id: "b1",
      deleted_at: deletedAt,
      deleted_by: { id: "alice", kind: "user" },
      id: `comment/${comment}#b1`,
      issue_key: "CORE-9",
      kind: "block",
      parent: { id: comment, kind: "comment", ref: `dispatch://CORE-9/comment/${comment}` },
      project: "CORE",
      ref: `dispatch://CORE-9/comment/${comment}#b-b1`,
    }),
    via: { id: "comment-1", kind: "comment" },
  });

  try {
    const row = await drawnRow();
    const link = within(row).getByRole("link", { hidden: true });
    expect(link).toHaveAttribute("href", `/issues/CORE-9/comments/${comment}#b-b1`);
    const line = link.querySelector("[data-post-deleted]");
    expect(line?.textContent).toMatch(/^Deleted by alice · /);
    expect(line?.querySelector("time")).toHaveAttribute("datetime", deletedAt);
    expect(row).not.toHaveTextContent("Ship it once the parser lands.");
    expect(row).toHaveTextContent("block · via comment");
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    getIssueReferences.mockRestore();
  }
});

test("a references read cut at its bound says References unavailable with a Retry that reads again", async () => {
  const user = userEvent.setup();
  const refusal =
    "the issue's reference closure ran past its 2s bound, so no reference was read; try again later";
  const { getIssueReferences, listArtifacts, view } = renderTab(
    [artifact],
    new ApiError(503, { code: "REFERENCES_TIMEOUT", error: refusal })
  );

  try {
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(`References unavailable: ${refusal}`);
    expect(screen.queryByText(/^References \(/)).toBeNull();
    getIssueReferences.mockResolvedValueOnce(noReferences);
    await user.click(within(alert).getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
    expect(getIssueReferences).toHaveBeenCalledTimes(2);
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    getIssueReferences.mockRestore();
  }
});

// A conversation's picture is owned by the agent whose conversation holds it: the References row
// names that agent through `AgentName`, a link to its live conversation. The answer has no
// `links`, as a Dispatch from before INFRA-4704 gives, so the row is a member's, with its depth.
test("From an answer without links, References names the agent that owns a conversation's artifact", async () => {
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([
    {
      capabilities: [],
      dir: "/w",
      host: "host",
      last_activity: null,
      last_seen: 1,
      machine_id: "host",
      open_asks: 0,
      roles: [],
      session_id: "planner-session",
      state: "live",
      title: "Planner",
    },
  ]);
  const picture: Artifact = {
    ...artifact,
    id: "artifact-shot",
    issue_key: null,
    name: "shot.png",
    project: "",
    session_id: "planner-session",
    slug: "shot-png",
  };
  const { getIssueReferences, listArtifacts, view } = renderTab([artifact], {
    members: [{ artifact: picture, depth: 1, via: { id: "comment-1", kind: "comment" } }],
    truncated: false,
  });

  try {
    const references = await screen.findByRole("region", { name: "References", hidden: true });
    const owner = await within(references).findByRole("link", { hidden: true, name: "Planner" });
    expect(owner).toHaveAttribute("href", "/agents/planner-session/live");
    expect(references).toHaveTextContent("agent Planner · via comment · depth 1");
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    getIssueReferences.mockRestore();
    listAgents.mockRestore();
  }
});

// The removal asks for nothing first: Dispatch removes only a document that holds nothing, so
// the press goes straight out and the row the person pressed on goes with it.
test("Remove on a document's row sends the delete and the row goes", async () => {
  const user = userEvent.setup();
  const notes: Artifact = {
    ...madeDocument,
    versions: artifact.versions.slice(0, 1),
  };
  const { getIssueReferences, listArtifacts, view } = renderTab([notes]);
  const deleteArtifact = spyOn(api, "deleteArtifact").mockImplementation(() => {
    listArtifacts.mockResolvedValue([]);
    return Promise.resolve();
  });

  try {
    const row = await screen.findByTestId("artifact-design-notes");
    await user.click(within(row).getByRole("button", { name: "Remove" }));

    await waitFor(() =>
      expect(deleteArtifact).toHaveBeenCalledWith("artifact-design-notes", keyedAttempt)
    );
    await waitFor(() => expect(screen.queryByTestId("artifact-design-notes")).toBeNull());
    expect(screen.getByText("No artifacts yet.")).not.toBeNull();
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    getIssueReferences.mockRestore();
    deleteArtifact.mockRestore();
  }
});

// A refusal is the server's own sentence, shown on the row where the person pressed, and the
// document stays.
test("a refused removal shows the server's sentence on the row", async () => {
  const user = userEvent.setup();
  const notes: Artifact = { ...madeDocument, versions: artifact.versions.slice(0, 1) };
  const { getIssueReferences, listArtifacts, view } = renderTab([notes]);
  const refusal =
    "Design notes is not untouched: it holds writing; a document is removed only while it holds nothing";
  const deleteArtifact = spyOn(api, "deleteArtifact").mockRejectedValue(
    new ApiError(409, { code: "ARTIFACT_NOT_UNTOUCHED", error: refusal })
  );

  try {
    const row = await screen.findByTestId("artifact-design-notes");
    await user.click(within(row).getByRole("button", { name: "Remove" }));

    expect(await within(row).findByRole("alert")).toHaveTextContent(refusal);
    expect(screen.getByTestId("artifact-design-notes")).not.toBeNull();
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    getIssueReferences.mockRestore();
    deleteArtifact.mockRestore();
  }
});

// `DispatchQueryClient` answers the invalidation of the idle document list after a removal with
// the list's own fetch, which TanStack pauses while the browser is offline, and no request
// deadline runs while a fetch is paused. So Remove's hold through that read has a timer of its
// own, the request deadline (`REQUEST_DEADLINE_MS`): a row's Remove whose removal landed offline
// is free again then, and the row says the list is not read yet and why. Once the network
// returns, the list is read and the row goes.
test("offline, a row's Remove whose removal landed is free again at the request deadline, and the row says why", async () => {
  const online = onlineManager.isOnline();
  const notes: Artifact = { ...madeDocument, versions: artifact.versions.slice(0, 1) };
  const queryClient = new DispatchQueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const { getIssueReferences, listArtifacts, view } = renderTab([notes], noReferences, {
    queryClient,
  });
  // The removal's answer, held until the case moves the clock, so what it sets off runs in `act`.
  const removal = Promise.withResolvers<void>();
  const deleteArtifact = spyOn(api, "deleteArtifact").mockImplementation(() => {
    listArtifacts.mockResolvedValue([]);
    return removal.promise;
  });
  const row = await screen.findByTestId("artifact-design-notes");
  const remove = within(row).getByRole("button", { name: "Remove" });
  const reads = listArtifacts.mock.calls.length;
  jest.useFakeTimers();
  try {
    const user = userOnFakeClock();
    onlineManager.setOnline(false);
    await user.click(remove);
    await tick(0);
    expect(deleteArtifact).toHaveBeenCalledTimes(1);
    removal.resolve();
    await tick(1_000);
    expect(queryClient.getQueryState(["artifacts", "CORE-1"])?.fetchStatus).toBe("paused");
    expect(remove.textContent).toBe("Removing…");
    // Held, never disabled, so a Remove pressed by keyboard keeps the focus: a press of it does
    // nothing.
    expect(remove).toHaveAttribute("aria-disabled", "true");
    expect(remove).not.toBeDisabled();
    await user.click(remove);
    await tick(0);
    expect(deleteArtifact).toHaveBeenCalledTimes(1);

    // The removal landed within the second before, which is when the hold's timer started.
    await tick(REQUEST_DEADLINE_MS - 2_000);
    expect(remove.textContent).toBe("Removing…");

    await tick(2_000);
    expect(remove.textContent).toBe("Remove");
    expect(remove).not.toHaveAttribute("aria-disabled");
    expect(listArtifacts.mock.calls.length).toBe(reads);
    expect(within(row).getByRole("alert").textContent).toBe(
      "Could not load artifacts. Dispatch could not be reached."
    );

    act(() => onlineManager.setOnline(true));
    await tick(0);
    expect(screen.queryByTestId("artifact-design-notes")).toBeNull();
  } finally {
    // The tab and its reads go before the network returns, so no read paused offline goes on.
    view.unmount();
    queryClient.clear();
    onlineManager.setOnline(online);
    realClock();
    listArtifacts.mockRestore();
    getIssueReferences.mockRestore();
    deleteArtifact.mockRestore();
  }
});

// The line a removal's row draws for a list read the bound ended is that removal's own: the next
// press of Remove takes it away as its removal lands, though the earlier read is still out, and
// the read after it, once answered, takes the row away. Dispatch holds the first removal's read
// until the row answers it, so the next removal's read, which waits for that one
// (`DispatchQueryClient`), answers after it.
test("the line a removal's read the bound ended draws goes as the next removal lands, whose read takes the row away", async () => {
  const notes: Artifact = { ...madeDocument, versions: artifact.versions.slice(0, 1) };
  const queryClient = new DispatchQueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const { getIssueReferences, listArtifacts, view } = renderTab([notes], noReferences, {
    queryClient,
  });
  // Each removal's answer, held until the row moves the clock, so what it sets off runs in `act`.
  const { answers: removals, next } = heldAnswers<void>();
  const deleteArtifact = spyOn(api, "deleteArtifact").mockImplementation(next);
  const row = await screen.findByTestId("artifact-design-notes");
  const remove = within(row).getByRole("button", { name: "Remove" });
  const reread = Promise.withResolvers<Artifact[]>();
  listArtifacts.mockResolvedValue([]);
  listArtifacts.mockReturnValueOnce(reread.promise);
  jest.useFakeTimers();
  try {
    const user = userOnFakeClock();
    await user.click(remove);
    await tick(0);
    removals[0]?.();
    await tick(0);
    expect(remove.textContent).toBe("Removing…");
    await tick(REQUEST_DEADLINE_MS);
    expect(remove.textContent).toBe("Remove");
    expect(within(row).getByRole("alert").textContent).toBe(
      "Could not load artifacts. Dispatch did not answer within 30 seconds."
    );

    await user.click(remove);
    await tick(0);
    expect(deleteArtifact).toHaveBeenCalledTimes(2);
    removals[1]?.();
    await tick(0);
    expect(remove.textContent).toBe("Removing…");
    expect(
      within(row).queryByRole("alert"),
      "the earlier removal's read kept its line past the next removal"
    ).toBeNull();

    await act(async () => reread.resolve([]));
    await tick(0);
    expect(screen.queryByTestId("artifact-design-notes")).toBeNull();
  } finally {
    view.unmount();
    queryClient.clear();
    realClock();
    listArtifacts.mockRestore();
    getIssueReferences.mockRestore();
    deleteArtifact.mockRestore();
  }
});

// Every removal reads the one document list again, so the line the first document's row draws
// for a read the bound ended goes as a second document's removal lands, though that read is still
// out: the list's one hold forgets it then (`useListReadBound`), and the second row alone says why
// the read its own removal made has not settled, once that read's bound ends.
test("the line one document's removal leaves goes as another document's removal lands", async () => {
  const notes: Artifact = { ...madeDocument, versions: artifact.versions.slice(0, 1) };
  const more: Artifact = {
    ...notes,
    id: "artifact-more-notes",
    name: "More notes",
    ref_key: "CORE-1/more-notes",
    slug: "more-notes",
  };
  const queryClient = new DispatchQueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const { getIssueReferences, listArtifacts, view } = renderTab([notes, more], noReferences, {
    queryClient,
  });
  // Each removal's answer, held until the row moves the clock, so what it sets off runs in `act`.
  const { answers: removals, next } = heldAnswers<void>();
  const deleteArtifact = spyOn(api, "deleteArtifact").mockImplementation(next);
  const firstRow = await screen.findByTestId("artifact-design-notes");
  const secondRow = screen.getByTestId("artifact-more-notes");
  const first = within(firstRow).getByRole("button", { name: "Remove" });
  const second = within(secondRow).getByRole("button", { name: "Remove" });
  const why = "Could not load artifacts. Dispatch did not answer within 30 seconds.";
  // Dispatch answers no read of the list after the first removal while the row runs.
  listArtifacts.mockReturnValue(Promise.withResolvers<Artifact[]>().promise);
  jest.useFakeTimers();
  try {
    const user = userOnFakeClock();
    await user.click(first);
    await tick(0);
    removals[0]?.();
    await tick(0);
    await tick(REQUEST_DEADLINE_MS);
    expect(within(firstRow).getByRole("alert").textContent).toBe(why);

    await user.click(second);
    await tick(0);
    expect(deleteArtifact).toHaveBeenCalledTimes(2);
    removals[1]?.();
    await tick(0);
    expect(second.textContent).toBe("Removing…");
    expect(
      within(firstRow).queryByRole("alert"),
      "the first removal's read kept its line on its row past the second removal"
    ).toBeNull();

    await tick(REQUEST_DEADLINE_MS);
    expect(within(secondRow).getByRole("alert").textContent).toBe(why);
    expect(within(firstRow).queryByRole("alert")).toBeNull();
  } finally {
    view.unmount();
    queryClient.clear();
    realClock();
    listArtifacts.mockRestore();
    getIssueReferences.mockRestore();
    deleteArtifact.mockRestore();
  }
});

// A file artifact is refused by the same test a document that holds writing is refused by, so
// its row offers no press that is always refused; a closed issue says why before the press.
test("a file's row offers no removal, and a closed issue refuses the document's", async () => {
  const notes: Artifact = { ...madeDocument, versions: artifact.versions.slice(0, 1) };
  const { getIssueReferences, listArtifacts, view } = renderTab([artifact, notes], noReferences, {
    isClosed: true,
  });

  try {
    const picture = await screen.findByTestId("artifact-diagram-png");
    expect(within(picture).queryByRole("button", { name: "Remove" })).not.toBeInTheDocument();
    const row = screen.getByTestId("artifact-design-notes");
    const remove = within(row).getByRole("button", { name: "Remove" });
    expect(remove).toHaveAttribute("aria-disabled", "true");
    expect(remove).toHaveAttribute(
      "title",
      "This issue is closed. Reopen it to remove this document."
    );
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    getIssueReferences.mockRestore();
  }
});
