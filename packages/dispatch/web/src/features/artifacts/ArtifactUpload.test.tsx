import { expect, type Mock, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";

import { ApiError, api } from "../../api/client";
import { ArtifactDropZone, ArtifactUploadRow, useArtifactUpload } from "./ArtifactUpload";

function TestHarness(): ReactNode {
  const upload = useArtifactUpload({ issue: "CORE-1" });
  return (
    <ArtifactDropZone upload={upload}>
      <ArtifactUploadRow upload={upload} />
    </ArtifactDropZone>
  );
}

function renderHarness() {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  return render(
    <QueryClientProvider client={queryClient}>
      <TestHarness />
    </QueryClientProvider>
  );
}

test("choosing a file stages it behind a summary field instead of uploading immediately", async () => {
  const user = userEvent.setup();
  const uploadArtifact = spyOn(api, "uploadArtifact");
  const view = renderHarness();

  try {
    const file = new File(["content"], "notes.md", { type: "text/markdown" });
    const input = screen.getByLabelText("Upload artifact");
    // Only a refused upload (a closed issue) disables the chooser.
    expect(input instanceof HTMLInputElement && input.disabled).toBe(false);
    await user.upload(input, [file]);

    expect(await screen.findByText("notes.md")).not.toBeNull();
    expect(screen.getByLabelText("Summary (optional)")).not.toBeNull();
    expect(uploadArtifact).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    uploadArtifact.mockRestore();
  }
});

test("confirming the staged file uploads it with the typed summary", async () => {
  const user = userEvent.setup();
  const uploadArtifact = spyOn(api, "uploadArtifact").mockResolvedValue({
    artifact: {
      created_at: "2026-09-11T00:00:00Z",
      created_by: { id: "alice", kind: "user" },
      id: "artifact-1",
      issue_key: "CORE-1",
      kind: "doc",
      name: "notes.md",
      primary: false,
      project: "CORE",
      slug: "notes-md",
      versions: [],
    },
    version: {
      authors: [{ id: "alice", kind: "user" }],
      created_at: "2026-09-11T00:00:00Z",
      named: false,
      number: 1,
      summary: "First draft",
    },
  });
  // The issue holds no notes.md yet, so this upload has no base to name.
  const ownerArtifactText = spyOn(api, "ownerArtifactText").mockRejectedValue(
    new ApiError(404, { code: "ARTIFACT_NOT_FOUND", error: "artifact not found" })
  );
  const view = renderHarness();

  try {
    const file = new File(["content"], "notes.md", { type: "text/markdown" });
    await user.upload(screen.getByLabelText("Upload artifact"), [file]);
    await screen.findByText("notes.md");
    await user.click(screen.getByLabelText("Summary (optional)"));
    await user.paste("First draft");
    await user.click(screen.getByRole("button", { name: "Upload" }));

    // The upload goes out once its file is kept in IndexedDB (`features/writes/write-store.ts`),
    // which is not the press's own tick, so the row waits for the call.
    await waitFor(() => expect(uploadArtifact).toHaveBeenCalledTimes(1));
    await screen.findByText("Upload");
    const [, input] = uploadArtifact.mock.calls[0] ?? [];
    expect(input?.name).toBe("notes.md");
    expect(input?.summary).toBe("First draft");
  } finally {
    view.unmount();
    uploadArtifact.mockRestore();
    ownerArtifactText.mockRestore();
  }
});

/** What Dispatch answers the uploads below, whose own fields none of them reads. */
const UPLOADED = {
  artifact: {
    created_at: "2026-09-11T00:00:00Z",
    created_by: { id: "alice", kind: "user" as const },
    id: "artifact-1",
    issue_key: "CORE-1",
    kind: "doc" as const,
    name: "notes.md",
    primary: false,
    project: "CORE",
    slug: "notes-md",
    versions: [],
  },
  version: {
    authors: [{ id: "alice", kind: "user" as const }],
    created_at: "2026-09-11T00:00:00Z",
    named: false,
    number: 2,
    summary: null,
  },
};

/** Stages `notes.md` and presses Upload, waiting until `text` has answered the base read. It
 *  drives the control as a person does (`userEvent`), as every other case in this file does. */
async function uploadNotes(text: Mock<typeof api.ownerArtifactText>): Promise<void> {
  const user = userEvent.setup();
  const file = new File(["content"], "notes.md", { type: "text/markdown" });
  await user.upload(screen.getByLabelText("Upload artifact"), [file]);
  await screen.findByText("notes.md");
  await user.click(screen.getByRole("button", { name: "Upload" }));
  await waitFor(() => expect(text).toHaveBeenCalledTimes(1));
}

test("an upload over a document the issue already holds names the base it replaces", async () => {
  const ownerArtifactText = spyOn(api, "ownerArtifactText").mockResolvedValue({
    markdown: "# Notes\n",
    token: "sha256:live-notes",
    version: 1,
  });
  const uploadArtifact = spyOn(api, "uploadArtifact").mockResolvedValue(UPLOADED);
  const view = renderHarness();

  try {
    await uploadNotes(ownerArtifactText);
    await waitFor(() => expect(uploadArtifact).toHaveBeenCalledTimes(1));
    // The name is read as Dispatch resolves an upload's: by the owner's own artifact route.
    expect(ownerArtifactText.mock.calls[0]).toEqual([{ issue: "CORE-1" }, "notes.md"]);
    const [, input] = uploadArtifact.mock.calls[0] ?? [];
    expect(input?.precondition).toEqual({ document: "sha256:live-notes" });
  } finally {
    view.unmount();
    uploadArtifact.mockRestore();
    ownerArtifactText.mockRestore();
  }
});

test("the first upload of a name has no base to name, and names none", async () => {
  const ownerArtifactText = spyOn(api, "ownerArtifactText").mockRejectedValue(
    new ApiError(404, { code: "ARTIFACT_NOT_FOUND", error: "artifact not found" })
  );
  const uploadArtifact = spyOn(api, "uploadArtifact").mockResolvedValue(UPLOADED);
  const view = renderHarness();

  try {
    await uploadNotes(ownerArtifactText);
    await waitFor(() => expect(uploadArtifact).toHaveBeenCalledTimes(1));
    const [, input] = uploadArtifact.mock.calls[0] ?? [];
    expect(input?.precondition).toBeUndefined();
  } finally {
    view.unmount();
    uploadArtifact.mockRestore();
    ownerArtifactText.mockRestore();
  }
});

// The base the dashboard reads for itself can be behind the live document by the time the upload
// lands — an agent writing that document, or another person uploading it — which is the race the
// base exists to catch, and the dashboard is now a caller that meets it. What the person reads
// then is whatever the server wrote, and the server writes for a caller holding the `dispatch`
// command: a person uploading in the browser has no command to run and no token to paste, so the
// row must say the document changed under them and what to do in the dashboard.
test("a refused upload tells the person the document changed, not a command to run", async () => {
  const ownerArtifactText = spyOn(api, "ownerArtifactText").mockResolvedValue({
    markdown: "# Notes\n",
    token: "sha256:stale-notes",
    version: 1,
  });
  const uploadArtifact = spyOn(api, "uploadArtifact").mockRejectedValue(
    // The sentence the server sends, read from a live Dispatch at this head:
    // POST /api/v1/issues/CORE-11/artifacts with a base behind the live document.
    new ApiError(409, {
      code: "PRECONDITION_FAILED",
      error:
        "document edit precondition failed: document changed; read the document with " +
        "`dispatch doc-read`, which prints `Document token: sha256:…`, and upload again naming " +
        'that token as `--precondition-json \'{"document":"sha256:…"}\'`',
    })
  );
  const view = renderHarness();

  try {
    await uploadNotes(ownerArtifactText);
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("changed");
    expect(alert.textContent).not.toContain("dispatch doc-read");
    expect(alert.textContent).not.toContain("--precondition-json");
  } finally {
    view.unmount();
    uploadArtifact.mockRestore();
    ownerArtifactText.mockRestore();
  }
});

// Dispatch stored the file and lost its answer (`409 KEY_COMMITTED`): the staged file and its
// summary go, the row says what was stored, and the row's Upload is the picker again, so a second
// press sends nothing.
test("an upload answered KEY_COMMITTED lets go of the staged file, says it was stored, and a second press sends nothing", async () => {
  const user = userEvent.setup();
  const uploadArtifact = spyOn(api, "uploadArtifact").mockRejectedValue(
    new ApiError(409, { code: "KEY_COMMITTED", error: "already made" })
  );
  const ownerArtifactText = spyOn(api, "ownerArtifactText").mockRejectedValue(
    new ApiError(404, { code: "ARTIFACT_NOT_FOUND", error: "artifact not found" })
  );
  const view = renderHarness();

  try {
    const file = new File(["content"], "notes.md", { type: "text/markdown" });
    await user.upload(screen.getByLabelText("Upload artifact"), [file]);
    await screen.findByText("notes.md");
    await user.click(screen.getByRole("button", { name: "Upload" }));

    const made = await screen.findByTestId("landed-without-answer");
    expect(made.textContent).toBe(
      "The upload of notes.md was made, but Dispatch lost its answer, so it may take a moment to appear."
    );
    expect(screen.queryByLabelText("Summary (optional)")).not.toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Upload" }));
    await Bun.sleep(20);
    expect(uploadArtifact).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    uploadArtifact.mockRestore();
    ownerArtifactText.mockRestore();
  }
});

test("cancel discards the staged file without uploading", async () => {
  const user = userEvent.setup();
  const uploadArtifact = spyOn(api, "uploadArtifact");
  const view = renderHarness();

  try {
    const file = new File(["content"], "notes.md", { type: "text/markdown" });
    await user.upload(screen.getByLabelText("Upload artifact"), [file]);
    await screen.findByText("notes.md");
    await user.click(screen.getByRole("button", { name: "Cancel" }));

    expect(screen.queryByText("notes.md")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Upload" })).not.toBeNull();
    expect(uploadArtifact).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    uploadArtifact.mockRestore();
  }
});
