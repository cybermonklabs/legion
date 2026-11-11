import { expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: this file keeps fireEvent for the form's own submit
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useLocation } from "react-router-dom";
import { realClock, tick } from "../../__tests__/fake-clock";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError } from "../../api/client";
import type { Artifact } from "../../api/types";
import { SendStoppedError } from "../writes/keyed-write";
import { DocumentList } from "./DocumentList";

function document(overrides: Partial<Artifact> = {}): Artifact {
  return {
    created_at: "2026-09-10T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    id: "artifact-1",
    issue_key: null,
    kind: "doc",
    name: "Design notes",
    primary: false,
    project: "CORE",
    ref_key: "CORE/design-notes",
    session_id: null,
    slug: "design-notes",
    versions: [
      {
        authors: [{ id: "alice", kind: "user" }],
        created_at: "2026-09-10T00:05:00Z",
        named: false,
        number: 1,
        summary: null,
      },
    ],
    ...overrides,
  };
}

function Location() {
  return <output data-testid="location">{useLocation().pathname}</output>;
}

function uploadResponse() {
  const artifact = document();
  const version = artifact.versions?.at(0);
  if (version === undefined) {
    throw new Error("document fixture requires a version");
  }
  return { artifact, version };
}

function renderList(artifacts: Artifact[]) {
  const listProjectArtifacts = spyOn(api, "listProjectArtifacts").mockResolvedValue(artifacts);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <DocumentList project="CORE" />
        <Location />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { listProjectArtifacts, queryClient, view };
}

test("Documents requests unlinked project artifacts and links a document to its project page", async () => {
  const { listProjectArtifacts, view } = renderList([document()]);

  try {
    const unlinked = await screen.findByRole("listitem", { name: /Design notes/ });
    expect(unlinked).toHaveTextContent("doc");
    expect(unlinked.querySelector("time")).not.toBeNull();
    expect(screen.getByRole("link", { name: "Design notes" })).toHaveAttribute(
      "href",
      "/projects/CORE/documents/design-notes"
    );
    expect(listProjectArtifacts).toHaveBeenCalledWith("CORE", true);
  } finally {
    view.unmount();
    listProjectArtifacts.mockRestore();
  }
});

test("a document's row names its latest version's stored size", async () => {
  const [first] = document().versions ?? [];
  if (first === undefined) {
    throw new Error("document fixture requires a version");
  }
  const latest = { ...first, number: 2, size: 1_048_475 };
  const { listProjectArtifacts, view } = renderList([
    document({ versions: [{ ...latest, number: 1, size: 12 }, latest] }),
  ]);

  try {
    const row = await screen.findByRole("listitem", { name: /Design notes/ });
    expect(row).toHaveTextContent("1023.9 KB");
    expect(row.textContent).not.toContain("12 B");
  } finally {
    view.unmount();
    listProjectArtifacts.mockRestore();
  }
});

test("New document opens its document page", async () => {
  const user = userEvent.setup();
  const { listProjectArtifacts, view } = renderList([]);
  const uploadArtifact = spyOn(api, "uploadArtifact").mockResolvedValue(uploadResponse());

  try {
    await screen.findByText("No documents yet.");
    await user.click(screen.getByRole("button", { name: "New document" }));
    await user.click(screen.getByRole("textbox", { name: "Title" }));
    await user.paste("Design notes");
    await user.click(screen.getByRole("button", { name: "Create" }));

    await waitFor(() =>
      expect(uploadArtifact).toHaveBeenCalledWith(
        { project: "CORE" },
        { content: "# Design notes\n", name: "Design notes" },
        keyedAttempt
      )
    );
    expect(screen.getByTestId("location").textContent).toBe(
      "/projects/CORE/documents/design-notes"
    );
  } finally {
    view.unmount();
    listProjectArtifacts.mockRestore();
    uploadArtifact.mockRestore();
  }
});

// Dispatch made the document and lost its answer (`409 KEY_COMMITTED`): there is no document page
// to open, so the form closes, says what was made and the list is read again - and the form opened
// again starts empty, so a second press makes no second document.
test("New document answered KEY_COMMITTED closes the form, says it was made, and a second press sends nothing", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const { listProjectArtifacts, view } = renderList([]);
  const uploadArtifact = spyOn(api, "uploadArtifact").mockRejectedValue(
    new ApiError(409, { code: "KEY_COMMITTED", error: "already made" })
  );

  try {
    await screen.findByText("No documents yet.");
    await user.click(screen.getByRole("button", { name: "New document" }));
    await user.click(screen.getByRole("textbox", { name: "Title" }));
    await user.paste("Design notes");
    await user.click(screen.getByRole("button", { name: "Create" }));

    const made = await screen.findByTestId("landed-without-answer");
    expect(made.textContent).toBe(
      "Document Design notes was made, but Dispatch lost its answer, so it may take a moment to appear."
    );
    expect(screen.queryByRole("form", { name: "New document" })).not.toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.getByTestId("location").textContent).toBe("/");
    await waitFor(() => expect(listProjectArtifacts.mock.calls.length).toBeGreaterThan(1));

    await user.click(screen.getByRole("button", { name: "New document" }));
    expect(screen.getByRole<HTMLInputElement>("textbox", { name: "Title" }).value).toBe("");
    // An empty form submitted on its own, which no control of it offers: Create is disabled.
    fireEvent.submit(screen.getByRole("form", { name: "New document" }));
    await tick(20);
    expect(uploadArtifact).toHaveBeenCalledTimes(1);
  } finally {
    realClock();
    view.unmount();
    listProjectArtifacts.mockRestore();
    uploadArtifact.mockRestore();
  }
});

test("New document refuses a name that already exists", async () => {
  const user = userEvent.setup();
  const { listProjectArtifacts, view } = renderList([document()]);
  const uploadArtifact = spyOn(api, "uploadArtifact").mockResolvedValue(uploadResponse());

  try {
    await screen.findByText("Design notes");
    await user.click(screen.getByRole("button", { name: "New document" }));
    await user.click(screen.getByRole("textbox", { name: "Title" }));
    await user.paste("Design notes");
    await user.click(screen.getByRole("button", { name: "Create" }));

    const error = await screen.findByRole("alert");
    expect(error).toHaveTextContent("A document named Design notes already exists");
    expect(uploadArtifact).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    listProjectArtifacts.mockRestore();
    uploadArtifact.mockRestore();
  }
});

// Dispatch refuses an upload under the name of a document that already exists unless it names
// the version it replaces, and a new document replaces nothing, so it names none: a title this
// list did not show as taken - a document created since the list was read, or one whose slug
// the title matches - is refused `400 PRECONDITION_REQUIRED`. The server writes that refusal
// for a caller holding the `dispatch` command; a person creating a document in the browser runs
// no command and pastes no token, so the form says the name is taken, in its own words.
test("a New document Dispatch refused over a name it holds says the name is taken", async () => {
  const user = userEvent.setup();
  const { listProjectArtifacts, view } = renderList([]);
  const uploadArtifact = spyOn(api, "uploadArtifact").mockRejectedValue(
    // The sentence the server sends, which it refuses every replacement naming no version with
    // (`api/artifacts.go` in the server).
    new ApiError(400, {
      code: "PRECONDITION_REQUIRED",
      error:
        'an upload replacing document "Design notes" must name the token of the version it ' +
        "replaces: read the document with `dispatch doc-read`, which prints " +
        "`Document token: sha256:…`, and upload again naming that token as " +
        '`--precondition-json \'{"document":"sha256:…"}\'`',
    })
  );

  try {
    await screen.findByText("No documents yet.");
    await user.click(screen.getByRole("button", { name: "New document" }));
    await user.click(screen.getByRole("textbox", { name: "Title" }));
    await user.paste("Design notes");
    await user.click(screen.getByRole("button", { name: "Create" }));

    const error = await screen.findByRole("alert");
    expect(error.textContent).toBe("A document named Design notes already exists");
    expect(error.textContent).not.toContain("dispatch doc-read");
    expect(error.textContent).not.toContain("--precondition-json");
  } finally {
    view.unmount();
    listProjectArtifacts.mockRestore();
    uploadArtifact.mockRestore();
  }
});

// Stop trying is no verdict of Dispatch's: the create may have reached it, and a press of Create
// again asks under the same key. The shell lets go of an inline write the reader stopped, so this
// form is the only place they read what became of it, and the words it shows are the stopped
// write's own - which say what a retry of it does. "Could not upload the artifact." names the
// wrong thing and promises nothing.
test("a New document the reader stopped says what the stopped write says", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const { listProjectArtifacts, queryClient, view } = renderList([]);
  const uploadArtifact = spyOn(api, "uploadArtifact").mockRejectedValue(
    new NetworkError("/api/v1/projects/CORE/artifacts", new TypeError("Failed to fetch"))
  );

  try {
    await tick(0);
    await user.click(screen.getByRole("button", { name: "New document" }));
    await user.click(screen.getByRole("textbox", { name: "Title" }));
    await user.paste("Design notes");
    await user.click(screen.getByRole("button", { name: "Create" }));
    await stopTheLateWrite(queryClient);

    expect(screen.getByRole("alert").textContent).toBe(new SendStoppedError().message);
    // The form stands, so Create presses again - under that same key, as those words say.
    expect(screen.getByRole("form", { name: "New document" })).toBeInTheDocument();
  } finally {
    view.unmount();
    listProjectArtifacts.mockRestore();
    uploadArtifact.mockRestore();
    realClock();
  }
});
