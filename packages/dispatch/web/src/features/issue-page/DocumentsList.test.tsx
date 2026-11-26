import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: a file dropped on the list, which user-event has no form for
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http } from "msw";
import { MemoryRouter, useLocation } from "react-router-dom";

import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { answerRequests } from "../../__tests__/network";
import { api } from "../../api/client";
import type { Artifact } from "../../api/types";
import { CLOSED_ISSUE_DOCUMENT_REFUSAL } from "../artifacts/CreateDocument";
import { buildIssuePath, parseIssuePath } from "../refs/routes";
import { DocumentsList } from "./DocumentsList";
import { CONTROL_LABELS, EMPTY_STATE_LINES, FILE_WORDS } from "./page-words";
import { SHOW_MORE, showMoreName } from "./show-more-fixture";

/**
 * Documents' list (dispatch://INFRA-1196 part N's R2): the issue page has no Artifacts tab and the
 * tree draws no document, so every document and file of the issue is a row of this list, which
 * the stage draws in place of the outline, and the control that makes a document from a title -
 * the Artifacts panel's `New document` (`CreateDocument.tsx`) - stands with it, beside the
 * upload, and reaches the same write. The cases the node menu held at the source's checkpoint
 * read the list's rows where they read the menu's roles (the plan's departure 6).
 */

/** A document of the issue: a row of the list, whose title New document refuses, since the
 *  write compares the title with every artifact. */
const background = {
  created_at: "2026-11-12T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-background",
  issue_key: "CORE-1",
  kind: "doc",
  name: "Background",
  primary: false,
  project: "CORE",
  ref_key: "CORE-1/background",
  session_id: null,
  slug: "background",
  versions: [
    {
      authors: [{ id: "alice", kind: "user" }],
      created_at: "2026-11-12T00:00:00Z",
      mime: "text/markdown",
      named: false,
      number: 1,
      sha256: "background-sha",
      size: 0,
      summary: null,
    },
  ],
} satisfies Artifact;

const made = {
  ...background,
  id: "artifact-design-notes",
  name: "Design notes",
  ref_key: "CORE-1/design-notes",
  slug: "design-notes",
} satisfies Artifact;

function Location() {
  return <output data-testid="location">{useLocation().pathname}</output>;
}

/** The list as the stage draws it: at its own address, and gone where one of its rows or New
 *  document takes the reader, as the stage draws the document there in its place. */
function List({
  artifacts,
  isClosed,
}: {
  readonly artifacts: readonly Artifact[];
  readonly isClosed: boolean;
}) {
  const route = parseIssuePath(useLocation().pathname);
  return route?.kind === "issue-documents" ? (
    <DocumentsList artifacts={artifacts} isClosed={isClosed} issueKey="CORE-1" />
  ) : null;
}

function renderList(
  isClosed = false,
  artifacts: readonly Artifact[] = [background],
  queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  })
) {
  return render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: "CORE-1", kind: "issue-documents" })]}>
      <QueryClientProvider client={queryClient}>
        <List artifacts={artifacts} isClosed={isClosed} />
        <Location />
      </QueryClientProvider>
    </MemoryRouter>
  );
}

/** The list's rows, in the order it draws them. */
function rows(): HTMLElement[] {
  return within(screen.getByRole("list")).getAllByRole("listitem");
}

/** The row whose link to its document is named `name`. */
function rowOf(name: string): HTMLElement | undefined {
  return rows().find((row) => within(row).queryByRole("link", { name }) !== null);
}

// Every artifact of the issue is a row of the list, named by its own name with its own copy
// control, and opens at its own address in the list's place.
test("Documents' list lists every document of the issue, each opening at its own address", async () => {
  const user = userEvent.setup();
  const view = renderList();
  try {
    const row = rowOf("Background");
    expect(row).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Copy reference dispatch://CORE-1/artifact/background" })
    ).toBeInTheDocument();
    await user.click(screen.getByRole("link", { name: "Background" }));
    expect(row).not.toBeInTheDocument();
    expect(screen.getByTestId("location").textContent).toBe("/issues/CORE-1/artifacts/background");
  } finally {
    view.unmount();
  }
});

// The control gives way to the title form, with the caret in its Title, and Create sends the
// title alone - so the document holds nothing - and takes the reader to it, on the page they are
// already on.
test("New document in Documents' list makes an empty document and opens it", async () => {
  const user = userEvent.setup();
  const uploadArtifact = spyOn(api, "uploadArtifact").mockResolvedValue({
    artifact: made,
    version: made.versions[0],
  });
  const view = renderList();
  try {
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.new_document }));
    expect(
      screen.queryByRole("button", { name: CONTROL_LABELS.new_document })
    ).not.toBeInTheDocument();
    const title = screen.getByRole("textbox", { name: "Title" });
    expect(
      document.activeElement,
      "the form opened with the caret somewhere other than its Title"
    ).toBe(title);
    await user.type(title, "Design notes");
    await user.click(screen.getByRole("button", { name: "Create" }));

    await waitFor(() =>
      expect(uploadArtifact).toHaveBeenCalledWith(
        { issue: "CORE-1" },
        { name: "Design notes" },
        keyedAttempt
      )
    );
    await waitFor(() =>
      expect(screen.getByTestId("location").textContent).toBe(
        "/issues/CORE-1/artifacts/design-notes"
      )
    );
    expect(screen.queryByRole("form", { name: "New document" })).not.toBeInTheDocument();
  } finally {
    view.unmount();
    uploadArtifact.mockRestore();
  }
});

// The title is held against every artifact of the issue, so a title the issue already holds is
// refused before the write, in the control's own words.
test("New document in Documents' list refuses a title the issue already holds", async () => {
  const user = userEvent.setup();
  const uploadArtifact = spyOn(api, "uploadArtifact");
  const view = renderList();
  try {
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.new_document }));
    await user.type(screen.getByRole("textbox", { name: "Title" }), "Background");
    await user.click(screen.getByRole("button", { name: "Create" }));

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "A document on this issue already holds that title: Background."
    );
    expect(uploadArtifact).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    uploadArtifact.mockRestore();
  }
});

// A closed issue takes no new document. The control stays in the list, refused, and says why in
// the sentence every way of adding to a closed issue says, as the Artifacts list's own control
// did; a press opens no form.
test("a closed issue's Documents' list draws New document refused, and says why", async () => {
  const user = userEvent.setup();
  const view = renderList(true);
  try {
    const control = screen.getByRole("button", { name: CONTROL_LABELS.new_document });
    expect(control).toHaveAttribute("aria-disabled", "true");
    expect(control).toHaveAccessibleDescription(CLOSED_ISSUE_DOCUMENT_REFUSAL);
    await user.click(control);
    expect(screen.queryByRole("form", { name: "New document" })).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

// A closed issue takes no upload either. The control is drawn refused, as New document is, and
// says why in the one sentence every way of adding to a closed issue says (`CreateDocument.tsx`'s
// `CLOSED_ISSUE_DOCUMENT_REFUSAL`); a press opens no chooser.
test("a closed issue's Documents' list draws Upload refused, and says why", async () => {
  const user = userEvent.setup();
  const view = renderList(true);
  try {
    const upload = screen.getByRole("button", { name: CONTROL_LABELS.upload });
    expect(upload).toHaveAttribute("aria-disabled", "true");
    expect(upload).toHaveAccessibleDescription(CLOSED_ISSUE_DOCUMENT_REFUSAL);
    expect(upload).toHaveTextContent(CLOSED_ISSUE_DOCUMENT_REFUSAL);
    const chooser = screen.getByLabelText(CONTROL_LABELS.upload_file);
    const opened = spyOn(chooser, "click");
    try {
      await user.click(upload);
      expect(opened).not.toHaveBeenCalled();
    } finally {
      opened.mockRestore();
    }
  } finally {
    view.unmount();
  }
});

// An open issue's Upload is a plain control that opens the chooser, with no refusal on it.
test("an open issue's Documents' list draws Upload as an item that opens the chooser", async () => {
  const user = userEvent.setup();
  const view = renderList(false);
  try {
    const upload = screen.getByRole("button", { name: CONTROL_LABELS.upload });
    expect(upload).not.toHaveAttribute("aria-disabled");
    expect(upload).not.toHaveAccessibleDescription();
    const chooser = screen.getByLabelText(CONTROL_LABELS.upload_file);
    const opened = spyOn(chooser, "click");
    try {
      await user.click(upload);
      expect(opened).toHaveBeenCalledTimes(1);
    } finally {
      opened.mockRestore();
    }
  } finally {
    view.unmount();
  }
});

// A picked file is staged, as the Artifacts list's upload staged it: the list names the file and
// offers an optional Summary for the version it makes, with Upload and Cancel, and nothing goes up
// before Upload. Cancel drops the pick and sends nothing.
test("a file picked from Documents' list is staged with its name, a Summary, Upload and Cancel", async () => {
  const user = userEvent.setup();
  const uploadArtifact = spyOn(api, "uploadArtifact").mockResolvedValue({
    artifact: made,
    version: made.versions[0],
  });
  const view = renderList(false);
  try {
    const chooser = screen.getByLabelText(CONTROL_LABELS.upload_file);
    const notes = new File(["# Notes\n"], "notes.md", { type: "text/markdown" });
    await user.upload(chooser, notes);
    expect(screen.getByText("notes.md")).toBeInTheDocument();
    expect(uploadArtifact).not.toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByText("notes.md")).not.toBeInTheDocument();
    expect(uploadArtifact).not.toHaveBeenCalled();

    await user.upload(chooser, notes);
    await user.type(screen.getByRole("textbox", { name: "Summary (optional)" }), "First notes");
    await user.click(screen.getByRole("button", { name: "Upload" }));
    await waitFor(() =>
      expect(uploadArtifact).toHaveBeenCalledWith(
        { issue: "CORE-1" },
        { file: notes, name: "notes.md", summary: "First notes" },
        keyedAttempt
      )
    );
  } finally {
    view.unmount();
    uploadArtifact.mockRestore();
  }
});

/** The spec, with two versions, and a picture: what the carried list draws for each kind. */
const spec = {
  ...background,
  id: "artifact-spec",
  name: "spec.md",
  primary: true,
  ref_key: "CORE-1/spec-md",
  slug: "spec-md",
  versions: [...background.versions, { ...background.versions[0], number: 2 }],
} satisfies Artifact;
const picture = {
  ...background,
  id: "artifact-shot",
  kind: "image",
  name: "shot.png",
  ref_key: "CORE-1/shot-png",
  slug: "shot-png",
} satisfies Artifact;

// The list carries the Artifacts list (the Design's "Where each thing goes"): the spec first,
// each row with the download of its latest version - `?download=true`, which the server answers
// under the file's own name - and the spec opened at its own address - `/spec` is the Spec
// view - so choosing it opens the document in place of the list.
test("each item downloads its latest version, and the spec opens at its own address", async () => {
  const user = userEvent.setup();
  const view = renderList(false, [background, spec]);
  try {
    const items = rows().map((item) => item.textContent ?? "");
    expect(items[0]).toContain("spec.md");
    expect(
      screen.getByRole("link", { name: `${CONTROL_LABELS.download_version} 2` })
    ).toHaveAttribute("href", "/api/v1/artifacts/artifact-spec/versions/2?download=true");
    expect(
      screen.getByRole("link", { name: `${CONTROL_LABELS.download_version} 1` })
    ).toHaveAttribute("href", "/api/v1/artifacts/artifact-background/versions/1?download=true");
    await user.click(screen.getByRole("link", { name: "spec.md" }));
    expect(screen.getByTestId("location").textContent).toBe("/issues/CORE-1/artifacts/spec-md");
  } finally {
    view.unmount();
  }
});

// Each row names its file's kind in a reader's word, and the spec carries the Primary mark the
// Artifacts list drew, so a reader still sees which document is the spec.
test("each item names its kind, and the spec carries the Primary mark", () => {
  const view = renderList(false, [background, spec, picture]);
  try {
    expect(rowOf("spec.md")).toHaveTextContent(FILE_WORDS.doc);
    expect(rowOf("spec.md")).toHaveTextContent(FILE_WORDS.primary);
    expect(rowOf("Background")).toHaveTextContent(FILE_WORDS.doc);
    expect(rowOf("Background")).not.toHaveTextContent(FILE_WORDS.primary);
    expect(rowOf("shot.png")).toHaveTextContent(FILE_WORDS.image);
  } finally {
    view.unmount();
  }
});

// The list's filter narrows its rows by name and by kind, as it narrowed the Artifacts list.
test("Documents' list's filter narrows the items by name and by kind", async () => {
  const user = userEvent.setup();
  const view = renderList(false, [background, picture]);
  try {
    const filter = screen.getByRole("searchbox", { name: CONTROL_LABELS.filter_files });
    await user.type(filter, "back");
    expect(screen.getByRole("link", { name: "Background" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "shot.png" })).not.toBeInTheDocument();
    await user.clear(filter);
    await user.type(filter, "image");
    expect(screen.getByRole("link", { name: "shot.png" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Background" })).not.toBeInTheDocument();
    await user.clear(filter);
    await user.type(filter, "nothing");
    expect(screen.getByText(EMPTY_STATE_LINES.files_filtered)).toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

// The kind a reader filters by is the word the row draws for it: "document" for a document, and
// "file" for a kind the page has no word of its own for, never the token the server stores.
test("Documents' list's filter matches each item's kind by the word its row draws", async () => {
  const handbook = {
    ...background,
    id: "artifact-handbook",
    kind: "pdf",
    name: "Handbook",
    ref_key: "CORE-1/handbook",
    slug: "handbook",
  } satisfies Artifact;
  const user = userEvent.setup();
  const view = renderList(false, [background, picture, handbook]);
  try {
    expect(rowOf("Handbook")).toHaveTextContent(FILE_WORDS.file);
    const filter = screen.getByRole("searchbox", { name: CONTROL_LABELS.filter_files });
    await user.type(filter, FILE_WORDS.doc);
    expect(rows().map((row) => within(row).getAllByRole("link")[0]?.textContent)).toEqual([
      "Background",
    ]);
    await user.clear(filter);
    await user.type(filter, FILE_WORDS.file);
    expect(rows().map((row) => within(row).getAllByRole("link")[0]?.textContent)).toEqual([
      "Handbook",
    ]);
    await user.clear(filter);
    await user.type(filter, "pdf");
    expect(screen.getByText(EMPTY_STATE_LINES.files_filtered)).toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

// Remove asks for nothing first: Dispatch removes only a document that holds nothing, so the
// press goes straight out. A file, the spec and a role document are refused whatever they hold,
// so no row offers it for one; on a closed issue it is refused, with its reason.
test("Remove sends the delete for a document the issue can drop, and nowhere else", async () => {
  const user = userEvent.setup();
  const deleteArtifact = spyOn(api, "deleteArtifact").mockResolvedValue();
  const view = renderList(false, [background, spec, picture]);
  try {
    expect(screen.getAllByRole("button", { name: "Remove" })).toHaveLength(1);
    await user.click(screen.getByRole("button", { name: "Remove" }));
    await waitFor(() =>
      expect(deleteArtifact).toHaveBeenCalledWith("artifact-background", keyedAttempt)
    );
  } finally {
    view.unmount();
    deleteArtifact.mockRestore();
  }
  const closed = renderList(true, [background]);
  try {
    expect(screen.getByRole("button", { name: "Remove" })).toHaveAttribute("aria-disabled", "true");
  } finally {
    closed.unmount();
  }
});

// A closed issue takes no file and no document from the list, by any way in: each control stays,
// refused, with the reason as its description; a press or Enter on Upload opens no chooser and on
// New document opens no form; a file dropped on the list is not staged; and nothing is sent.
test("a closed issue refuses Upload and New document and says why", async () => {
  const user = userEvent.setup();
  const uploadArtifact = spyOn(api, "uploadArtifact");
  const view = renderList(true);
  try {
    const upload = screen.getByRole("button", { name: CONTROL_LABELS.upload });
    const newDocument = screen.getByRole("button", { name: CONTROL_LABELS.new_document });
    for (const control of [upload, newDocument]) {
      expect(control).toHaveAttribute("aria-disabled", "true");
      expect(control).toHaveAccessibleDescription(CLOSED_ISSUE_DOCUMENT_REFUSAL);
      expect(control).toHaveTextContent(CLOSED_ISSUE_DOCUMENT_REFUSAL);
    }
    const chooser = screen.getByLabelText(CONTROL_LABELS.upload_file);
    expect(chooser).toBeDisabled();
    const opened = spyOn(chooser, "click");
    try {
      await user.click(upload);
      upload.focus();
      await user.keyboard("{Enter}");
      expect(opened).not.toHaveBeenCalled();
    } finally {
      opened.mockRestore();
    }
    newDocument.focus();
    await user.keyboard("{Enter}");
    await user.click(newDocument);
    expect(screen.queryByRole("form", { name: "New document" })).not.toBeInTheDocument();
    expect(screen.queryByRole("textbox", { name: "Title" })).not.toBeInTheDocument();

    const notes = new File(["# Notes\n"], "notes.md", { type: "text/markdown" });
    // A drop of files, which user-event has no form for.
    fireEvent.drop(screen.getByRole("list"), { dataTransfer: { files: [notes] } });
    expect(screen.queryByText("notes.md")).not.toBeInTheDocument();
    expect(screen.queryByRole("textbox", { name: "Summary (optional)" })).not.toBeInTheDocument();
    expect(uploadArtifact).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    uploadArtifact.mockRestore();
  }
});

/** A document the issue holds under the name of a file a reader uploads: the upload replaces it. */
const notesDocument = {
  ...background,
  id: "artifact-notes",
  name: "notes.md",
  ref_key: "CORE-1/notes-md",
  slug: "notes-md",
} satisfies Artifact;

// An upload under the name of a document the issue holds replaces that document, and Dispatch
// takes it only where it names the version it replaces (`main`'s `useArtifactUpload`): the list
// reads that version's token as the file is picked, and Upload sends it as the upload's base.
test("Upload carries the replaced version's token", async () => {
  const user = userEvent.setup();
  const ownerArtifactText = spyOn(api, "ownerArtifactText").mockResolvedValue({
    markdown: "# Notes\n",
    token: "sha256:live-notes",
    version: 1,
  });
  const uploadArtifact = spyOn(api, "uploadArtifact").mockResolvedValue({
    artifact: notesDocument,
    version: notesDocument.versions[0],
  });
  const view = renderList(false, [background, notesDocument]);
  try {
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.upload }));
    const notes = new File(["# Notes, again\n"], "notes.md", { type: "text/markdown" });
    await user.upload(screen.getByLabelText(CONTROL_LABELS.upload_file), notes);
    await waitFor(() =>
      expect(ownerArtifactText).toHaveBeenCalledWith({ issue: "CORE-1" }, "notes.md")
    );
    expect(uploadArtifact).not.toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: "Upload" }));
    await waitFor(() =>
      expect(uploadArtifact).toHaveBeenCalledWith(
        { issue: "CORE-1" },
        {
          file: notes,
          name: "notes.md",
          precondition: { document: "sha256:live-notes" },
          summary: undefined,
        },
        keyedAttempt
      )
    );
    expect(uploadArtifact).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    uploadArtifact.mockRestore();
    ownerArtifactText.mockRestore();
  }
});

/** 25 documents of the issue, each of its own name. */
const twentyFive: readonly Artifact[] = Array.from({ length: 25 }, (_, index) => {
  const number = String(index + 1).padStart(2, "0");
  return {
    ...background,
    id: `artifact-note-${number}`,
    name: `Note ${number}`,
    ref_key: `CORE-1/note-${number}`,
    slug: `note-${number}`,
  } satisfies Artifact;
});

// R2 and root R40: the list draws 20 rows and, under them, the control that says how many are
// left; its press draws the rest and puts the focus on the first row it drew, and the control
// goes with nothing left. The filter's words start the pages again, 20 at a time.
test("the list draws 20 rows and a control that names how many are left, and the control draws the next 20", async () => {
  const user = userEvent.setup();
  const view = renderList(false, twentyFive);
  try {
    expect(rows()).toHaveLength(20);
    const more = screen.getByRole("button", { name: SHOW_MORE });
    expect(more.textContent).toBe(showMoreName(5));
    await user.click(more);
    expect(rows()).toHaveLength(25);
    const twentyFirst = rows()[20];
    if (twentyFirst === undefined) throw new Error("the list draws no 21st row");
    await waitFor(() =>
      expect(document.activeElement).toBe(within(twentyFirst).getByRole("link", { name: /^Note/ }))
    );
    expect(screen.queryByRole("button", { name: SHOW_MORE })).not.toBeInTheDocument();

    await user.type(screen.getByRole("searchbox", { name: CONTROL_LABELS.filter_files }), "note");
    expect(rows()).toHaveLength(20);
    expect(screen.getByRole("button", { name: SHOW_MORE }).textContent).toBe(showMoreName(5));
  } finally {
    view.unmount();
  }
});

/** A document whose second version came eight months after its first. */
const revised = {
  ...background,
  versions: [
    { ...background.versions[0], created_at: "2026-03-02T12:00:00Z" },
    { ...background.versions[0], created_at: "2026-11-12T12:00:00Z", number: 2 },
  ],
} satisfies Artifact;

// R2: each row names its version and its last change, which are its latest version's: its number
// and the day it was made, as the opened document's head names them.
test("each row names its version and its last change", () => {
  const view = renderList(false, [revised]);
  try {
    const row = rowOf("Background");
    expect(row).toHaveTextContent(`${FILE_WORDS.version} 2 · 12 Nov`);
    expect(row).not.toHaveTextContent("2 Mar");
  } finally {
    view.unmount();
  }
});

// R2: no menu of the page lists a document. Nothing the list draws has a menu's role or opens
// one, with a file staged and with New document's form open as on its first draw, and on a
// closed issue.
test("no element of the list has a menu's role", async () => {
  const user = userEvent.setup();
  const menus = () => document.querySelectorAll('[role^="menu"], [aria-haspopup="menu"]').length;
  const view = renderList(false, [background, spec, picture]);
  try {
    expect(rows()).toHaveLength(3);
    expect(menus(), "on its first draw").toBe(0);
    await user.upload(
      screen.getByLabelText(CONTROL_LABELS.upload_file),
      new File(["# Notes\n"], "notes.md", { type: "text/markdown" })
    );
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.new_document }));
    expect(screen.getByRole("form", { name: "New document" })).toBeInTheDocument();
    expect(menus(), "with a file staged and New document's form open").toBe(0);
  } finally {
    view.unmount();
  }
  const closed = renderList(true, [background]);
  try {
    expect(rows()).toHaveLength(1);
    expect(menus(), "on a closed issue").toBe(0);
  } finally {
    closed.unmount();
  }
});

// R2: the list draws the issue read's `artifacts`, which the page holds already, and reads
// nothing of its own: no request goes out as it is drawn, and it asks the cache for nothing.
test("the list makes no read of its own", async () => {
  const reads: string[] = [];
  answerRequests(
    http.all("*", ({ request }) => {
      reads.push(`${request.method} ${new URL(request.url).pathname}`);
      return Response.json([]);
    })
  );
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = renderList(false, [background, spec, picture], client);
  try {
    expect(rows()).toHaveLength(3);
    // The effects of the draw run, and any read they would start goes out, before the reading.
    await act(async () => {
      const { promise, resolve } = Promise.withResolvers<void>();
      setTimeout(resolve, 50);
      await promise;
    });
    expect(reads).toEqual([]);
    expect(
      client
        .getQueryCache()
        .getAll()
        .map((query) => query.queryKey)
    ).toEqual([]);
  } finally {
    view.unmount();
  }
});

// Part N's R2 and R4: where the issue holds no document and no file, the list says so in one
// line, as every other control of the view row says where it draws nothing, and draws no row;
// its filter's line is for words that match none of what the issue holds.
test("an issue with no document draws the list's empty line", async () => {
  const user = userEvent.setup();
  const view = renderList(false, []);
  try {
    expect(screen.getByText(EMPTY_STATE_LINES.documents)).toBeInTheDocument();
    expect(screen.queryByRole("list")).toBeNull();
    expect(screen.queryByText(EMPTY_STATE_LINES.files_filtered)).toBeNull();
    await user.type(
      screen.getByRole("searchbox", { name: CONTROL_LABELS.filter_files }),
      "nothing"
    );
    expect(screen.getByText(EMPTY_STATE_LINES.documents)).toBeInTheDocument();
    expect(screen.queryByText(EMPTY_STATE_LINES.files_filtered)).toBeNull();
  } finally {
    view.unmount();
  }
  // An issue that holds one draws no such line.
  const held = renderList(false, [background]);
  try {
    expect(screen.queryByText(EMPTY_STATE_LINES.documents)).toBeNull();
  } finally {
    held.unmount();
  }
});
