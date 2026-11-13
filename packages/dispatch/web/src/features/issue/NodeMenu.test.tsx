import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { api } from "../../api/client";
import type { Artifact } from "../../api/types";
import { CLOSED_ISSUE_DOCUMENT_REFUSAL } from "../artifacts/CreateDocument";
import { buildIssuePath } from "../refs/routes";
import { NodeMenu } from "./NodeMenu";
import { CONTROL_LABELS, EMPTY_STATE_LINES, FILE_WORDS } from "./page-words";

/**
 * The node menu (dispatch://INFRA-1196 R3): the issue page has no Artifacts tab and the tree
 * draws no document, so every document and artifact of the issue is an item of this menu, and
 * the control that makes a document from a title - the Artifacts panel's `New document`
 * (`CreateDocument.tsx`) - is an item of it too, beside the upload, and reaches the same write.
 */

/** A document of the issue: an item of the menu, whose title New document refuses, since the
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

/** The menu as the preset row holds it: open, and closed by the item that acts. */
function Menu({
  artifacts,
  isClosed,
}: {
  readonly artifacts: readonly Artifact[];
  readonly isClosed: boolean;
}) {
  const [open, setOpen] = useState(true);
  return (
    <NodeMenu
      artifacts={artifacts}
      isClosed={isClosed}
      issueKey="CORE-1"
      onClose={() => setOpen(false)}
      onToggle={() => setOpen(!open)}
      open={open}
    />
  );
}

function renderMenu(isClosed = false, artifacts: readonly Artifact[] = [background]) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  return render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: "CORE-1", kind: "artifacts" })]}>
      <QueryClientProvider client={queryClient}>
        <Menu artifacts={artifacts} isClosed={isClosed} />
        <Location />
      </QueryClientProvider>
    </MemoryRouter>
  );
}

// Every artifact of the issue is an item of the menu, named by its own name with its own copy
// control, and opens at its own address.
test("the node menu lists every document of the issue, each opening at its own address", async () => {
  const user = userEvent.setup();
  const view = renderMenu();
  try {
    const item = screen.getByRole("menuitem", { name: /^Background/ });
    expect(
      screen.getByRole("button", { name: "Copy reference dispatch://CORE-1/artifact/background" })
    ).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Background" }));
    expect(item).not.toBeInTheDocument();
    expect(screen.getByTestId("location").textContent).toBe("/issues/CORE-1/artifacts/background");
  } finally {
    view.unmount();
  }
});

// The item closes the menu and opens the title form beside it with the caret in its Title, and
// Create sends the title alone - so the document holds nothing - and takes the reader to it, on
// the page they are already on.
test("New document in the node menu makes an empty document and opens it", async () => {
  const user = userEvent.setup();
  const uploadArtifact = spyOn(api, "uploadArtifact").mockResolvedValue({
    artifact: made,
    version: made.versions[0],
  });
  const view = renderMenu();
  try {
    await user.click(screen.getByRole("menuitem", { name: CONTROL_LABELS.new_document }));
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
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
test("New document in the node menu refuses a title the issue already holds", async () => {
  const user = userEvent.setup();
  const uploadArtifact = spyOn(api, "uploadArtifact");
  const view = renderMenu();
  try {
    await user.click(screen.getByRole("menuitem", { name: CONTROL_LABELS.new_document }));
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

// A closed issue takes no new document. The item stays in the menu, refused, and says why in
// the sentence every way of adding to a closed issue says, as the Artifacts list's own control
// did; a press opens no form.
test("a closed issue's node menu draws New document refused, and says why", async () => {
  const user = userEvent.setup();
  const view = renderMenu(true);
  try {
    const item = screen.getByRole("menuitem", { name: CONTROL_LABELS.new_document });
    expect(item).toHaveAttribute("aria-disabled", "true");
    expect(item).toHaveAccessibleDescription(CLOSED_ISSUE_DOCUMENT_REFUSAL);
    await user.click(item);
    expect(screen.queryByRole("form", { name: "New document" })).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

// A closed issue takes no upload either. The item is drawn refused, as New document is, and says
// why in the one sentence every way of adding to a closed issue says (`CreateDocument.tsx`'s
// `CLOSED_ISSUE_DOCUMENT_REFUSAL`); a press opens no chooser.
test("a closed issue's node menu draws Upload refused, and says why", async () => {
  const user = userEvent.setup();
  const view = renderMenu(true);
  try {
    const upload = screen.getByRole("menuitem", { name: CONTROL_LABELS.upload });
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

// An open issue's Upload is a plain item that opens the chooser, with no refusal on it.
test("an open issue's node menu draws Upload as an item that opens the chooser", async () => {
  const user = userEvent.setup();
  const view = renderMenu(false);
  try {
    const upload = screen.getByRole("menuitem", { name: CONTROL_LABELS.upload });
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

// A picked file is staged, as the Artifacts list's upload staged it: the menu names the file and
// offers an optional Summary for the version it makes, with Upload and Cancel, and nothing goes up
// before Upload. Cancel drops the pick and sends nothing.
test("a file picked from the node menu is staged with its name, a Summary, Upload and Cancel", async () => {
  const user = userEvent.setup();
  const uploadArtifact = spyOn(api, "uploadArtifact").mockResolvedValue({
    artifact: made,
    version: made.versions[0],
  });
  const view = renderMenu(false);
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

// The menu carries the Artifacts list (the Design's "Where each thing goes"): the spec first,
// each item with the download of its latest version, and the spec opened at its own address -
// `/spec` is the Spec filter - so choosing it opens the document in place of the outline.
test("each item downloads its latest version, and the spec opens at its own address", async () => {
  const user = userEvent.setup();
  const view = renderMenu(false, [background, spec]);
  try {
    const items = screen.getAllByRole("menuitem").map((item) => item.textContent ?? "");
    expect(items[0]).toContain("spec.md");
    expect(
      screen.getByRole("link", { name: `${CONTROL_LABELS.download_version} 2` })
    ).toHaveAttribute("href", "/api/v1/artifacts/artifact-spec/versions/2");
    expect(
      screen.getByRole("link", { name: `${CONTROL_LABELS.download_version} 1` })
    ).toHaveAttribute("href", "/api/v1/artifacts/artifact-background/versions/1");
    await user.click(screen.getByRole("button", { name: "spec.md" }));
    expect(screen.getByTestId("location").textContent).toBe("/issues/CORE-1/artifacts/spec-md");
  } finally {
    view.unmount();
  }
});

// Each item names its file's kind in a reader's word, and the spec carries the Primary mark the
// Artifacts list drew, so a reader still sees which document is the spec.
test("each item names its kind, and the spec carries the Primary mark", () => {
  const view = renderMenu(false, [background, spec, picture]);
  try {
    const itemOf = (name: string) =>
      screen
        .getAllByRole("menuitem")
        .find((item) => within(item).queryByRole("button", { name }) !== null);
    expect(itemOf("spec.md")).toHaveTextContent(FILE_WORDS.doc);
    expect(itemOf("spec.md")).toHaveTextContent(FILE_WORDS.primary);
    expect(itemOf("Background")).toHaveTextContent(FILE_WORDS.doc);
    expect(itemOf("Background")).not.toHaveTextContent(FILE_WORDS.primary);
    expect(itemOf("shot.png")).toHaveTextContent(FILE_WORDS.image);
  } finally {
    view.unmount();
  }
});

// The list's filter narrows the menu by name and by kind, as it narrowed the list.
test("the node menu's filter narrows the items by name and by kind", async () => {
  const user = userEvent.setup();
  const view = renderMenu(false, [background, picture]);
  try {
    const filter = screen.getByRole("searchbox", { name: CONTROL_LABELS.filter_files });
    await user.type(filter, "back");
    expect(screen.getByRole("button", { name: "Background" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "shot.png" })).not.toBeInTheDocument();
    await user.clear(filter);
    await user.type(filter, "image");
    expect(screen.getByRole("button", { name: "shot.png" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Background" })).not.toBeInTheDocument();
    await user.clear(filter);
    await user.type(filter, "nothing");
    expect(screen.getByText(EMPTY_STATE_LINES.files_filtered)).toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

// Remove asks for nothing first: Dispatch removes only a document that holds nothing, so the
// press goes straight out. A file, the spec and a role document are refused whatever they hold,
// so no item offers it for one; on a closed issue it is refused, with its reason.
test("Remove sends the delete for a document the issue can drop, and nowhere else", async () => {
  const user = userEvent.setup();
  const deleteArtifact = spyOn(api, "deleteArtifact").mockResolvedValue();
  const view = renderMenu(false, [background, spec, picture]);
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
  const closed = renderMenu(true, [background]);
  try {
    expect(screen.getByRole("button", { name: "Remove" })).toHaveAttribute("aria-disabled", "true");
  } finally {
    closed.unmount();
  }
});
