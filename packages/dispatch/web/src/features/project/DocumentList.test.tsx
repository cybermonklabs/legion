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
import { stoppedCreate } from "../artifacts/CreateDocument";
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

// The create sends the title alone: the new document holds nothing, so the heading a person
// never typed is not there to delete.
test("New document makes an empty document and opens its document page", async () => {
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
        { name: "Design notes" },
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

test("New document refuses a title the project already holds, as a name and as a slug", async () => {
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
    expect(error).toHaveTextContent(
      "A document on this project already holds that title: Design notes."
    );

    // The write reads the owner's slugs too (`$2 in (name, slug)`), so the control refuses the
    // slug of a document it lists and names the document that holds it.
    await user.clear(screen.getByRole("textbox", { name: "Title" }));
    await user.click(screen.getByRole("textbox", { name: "Title" }));
    await user.paste("design-notes");
    await user.click(screen.getByRole("button", { name: "Create" }));

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "A document on this project already holds that title: Design notes."
    );
    expect(uploadArtifact).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    listProjectArtifacts.mockRestore();
    uploadArtifact.mockRestore();
  }
});

// New document sends the title alone, so a title this list did not show as taken - a document
// created since the list was read, or one whose slug the title matches - reaches Dispatch as a
// write with no content over a document that exists. Dispatch refuses that write whatever token
// it names (`blankWriteToADocumentThatExists`, `api/artifacts.go`), before it reads any token, so
// the person reads that refusal, whose remedies Dispatch takes, and never the token refusal of a
// replacement, whose remedy - name the token - would be refused in its turn.
test("a New document over a title Dispatch holds reads the blank write's refusal, not a token's", async () => {
  const user = userEvent.setup();
  const { listProjectArtifacts, view } = renderList([]);
  const refusal =
    '"Design notes" names artifact design-notes (Design notes), which exists; a write with no ' +
    "content makes no document and writes no version: edit it with dispatch doc-edit, or give " +
    "the new document another name";
  const uploadArtifact = spyOn(api, "uploadArtifact").mockRejectedValue(
    new ApiError(400, { code: "ARTIFACT_INPUT", error: refusal })
  );

  try {
    await screen.findByText("No documents yet.");
    await user.click(screen.getByRole("button", { name: "New document" }));
    await user.click(screen.getByRole("textbox", { name: "Title" }));
    await user.paste("Design notes");
    await user.click(screen.getByRole("button", { name: "Create" }));

    const error = await screen.findByRole("alert");
    expect(error.textContent).toBe(refusal);
    // And the form around it adds no remedy of its own that names a token, which the route would
    // refuse for this write whatever token it named.
    const form = screen.getByRole("form", { name: "New document" });
    expect(form.textContent).not.toMatch(/token|--precondition-json/i);
    // The write carries the title and nothing else: no content, so Dispatch reads it as a blank
    // write, and no token, which a new document has none of to name.
    const sent = uploadArtifact.mock.calls[0]?.[1];
    expect(sent).toEqual({ name: "Design notes" });
  } finally {
    view.unmount();
    listProjectArtifacts.mockRestore();
    uploadArtifact.mockRestore();
  }
});

// Stop trying is no verdict of Dispatch's: the create may have reached it. The shell lets go of an
// inline write the reader stopped, so this form is the only place they read what became of it.
// The stopped write's own words promise a Retry under its key, which this form does not make, so
// the form says what a press of Create again does. "Could not create the document." would name
// no outcome at all.
test("a New document the reader stopped says what pressing Create again does", async () => {
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

    expect(screen.getByRole("alert").textContent).toBe(stoppedCreate("Design notes"));
    // The form stands, with the title in it, for the person to press Create again.
    expect(screen.getByRole("form", { name: "New document" })).toBeInTheDocument();
  } finally {
    view.unmount();
    listProjectArtifacts.mockRestore();
    uploadArtifact.mockRestore();
    realClock();
  }
});

// A create the reader stopped belongs to the press that made it, and this form offers no Retry
// of it: Create pressed again is a press the person made anew, so it goes under a key of its own
// though it carries the same title. Sent under the stopped press's key, it would be answered with
// whatever that press did at Dispatch rather than decided as the press the person made now.
test("Create pressed again with the title a stopped create carried goes under a key of its own", async () => {
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
    const stoppedKey = uploadArtifact.mock.calls[0]?.[2]?.key;
    const stopped = uploadArtifact.mock.calls.length;

    uploadArtifact.mockResolvedValue(uploadResponse());
    await user.click(screen.getByRole("button", { name: "Create" }));
    await tick(1_000);

    expect(uploadArtifact).toHaveBeenCalledTimes(stopped + 1);
    const again = uploadArtifact.mock.calls.at(-1);
    expect(again?.[1]).toEqual({ name: "Design notes" });
    expect(again?.[2]?.key).not.toBe(stoppedKey);
  } finally {
    view.unmount();
    listProjectArtifacts.mockRestore();
    uploadArtifact.mockRestore();
    realClock();
  }
});

// A project's document is not removed: its events are its own rows, so its removal event would
// need the row the removal deletes. The list says so by offering no such control on any row.
test("the project document list shows no removal control", async () => {
  const { listProjectArtifacts, view } = renderList([
    document(),
    document({ id: "artifact-2", name: "Goals", ref_key: "CORE/goals", slug: "goals" }),
  ]);

  try {
    const rows = await screen.findAllByRole("listitem");
    expect(rows).toHaveLength(2);
    expect(screen.queryByRole("button", { name: "Remove" })).not.toBeInTheDocument();
  } finally {
    view.unmount();
    listProjectArtifacts.mockRestore();
  }
});
