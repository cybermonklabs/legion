import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { api } from "../../api/client";
import type { Artifact } from "../../api/types";
import { CLOSED_ISSUE_DOCUMENT_REFUSAL } from "../artifacts/CreateDocument";
import { buildIssuePath } from "../refs/routes";
import { NodeMenu } from "./NodeMenu";
import { CONTROL_LABELS } from "./page-words";

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
function Menu({ isClosed }: { readonly isClosed: boolean }) {
  const [open, setOpen] = useState(true);
  return (
    <NodeMenu
      artifacts={[background]}
      isClosed={isClosed}
      issueKey="CORE-1"
      onClose={() => setOpen(false)}
      onToggle={() => setOpen(!open)}
      open={open}
    />
  );
}

function renderMenu(isClosed = false) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  return render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: "CORE-1", kind: "artifacts" })]}>
      <QueryClientProvider client={queryClient}>
        <Menu isClosed={isClosed} />
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

// A closed issue takes no new document. The issue page offers no write a closed issue refuses
// (`IssueAddMenu.tsx`, `DecisionActions.tsx`), so the node menu draws no New document there;
// the Artifacts panel's own control is where the refusal is said (`ArtifactsTab.tsx`).
test("a closed issue's node menu offers no New document", () => {
  const view = renderMenu(true);
  try {
    expect(screen.getByRole("menuitem", { name: CONTROL_LABELS.upload })).toBeInTheDocument();
    expect(
      screen.queryByRole("menuitem", { name: CONTROL_LABELS.new_document })
    ).not.toBeInTheDocument();
    expect(screen.queryByRole("form", { name: "New document" })).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

// A closed issue takes no upload either, and the item said nothing: a press opened no chooser
// and drew no word. The item is drawn refused, as the Artifacts panel's own Upload is on a
// closed issue, and says why in the one sentence that panel says for both of its controls
// (`ArtifactsTab.tsx`, `CreateDocument.tsx`'s `CLOSED_ISSUE_DOCUMENT_REFUSAL`).
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
