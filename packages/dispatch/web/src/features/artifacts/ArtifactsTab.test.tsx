import { expect, jest, spyOn, test } from "bun:test";
import { onlineManager, QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: the dropped file below is a drop, which user-event has no form for
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useLocation } from "react-router-dom";

import { realClock, tick } from "../../__tests__/fake-clock";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, REQUEST_DEADLINE_MS } from "../../api/client";
import { DispatchQueryClient } from "../../api/query-client";
import type { Artifact, ArtifactUploadResponse, IssueReferences } from "../../api/types";
import { buildIssuePath } from "../refs/routes";
import { ArtifactsTab } from "./ArtifactsTab";

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

const noReferences: IssueReferences = { members: [], truncated: false };

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
  references: IssueReferences | Error = noReferences,
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

test("References lists closure members with chip, via, and depth, names truncation, and renders an artifact source without an issue key, collapsed by default", async () => {
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
// names that agent through `AgentName`, a link to its live conversation.
test("References names the agent that owns a conversation's artifact", async () => {
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
    expect(remove).toBeDisabled();

    // The removal landed within the second before, which is when the hold's timer started.
    await tick(REQUEST_DEADLINE_MS - 2_000);
    expect(remove.textContent).toBe("Removing…");

    await tick(2_000);
    expect(remove.textContent).toBe("Remove");
    expect(remove).not.toBeDisabled();
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
