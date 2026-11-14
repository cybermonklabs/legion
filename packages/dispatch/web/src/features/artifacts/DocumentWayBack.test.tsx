import { expect, spyOn, test } from "bun:test";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { answerArtifactRemovals, answerArtifactRenames } from "../../__tests__/send-answers";
import { QueryClient, QueryClientProvider, useQuery } from "../../api/cache/test-client";
import { ApiError, api } from "../../api/client";
import type { Artifact } from "../../api/types";
import type { DocumentToolbar } from "../doc/ProofDocument";
import { buildIssuePath } from "../refs/routes";
import { ArtifactHeader } from "./ArtifactHeader";

function document(overrides: Partial<Artifact> = {}): Artifact {
  return {
    created_at: "2026-11-10T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    id: "artifact-1",
    issue_key: "CORE-1",
    kind: "doc",
    name: "Lauch notes",
    primary: false,
    project: "CORE",
    ref_key: "CORE-1/lauch-notes",
    session_id: null,
    slug: "lauch-notes",
    versions: [
      {
        authors: [{ id: "alice", kind: "user" }],
        created_at: "2026-11-10T00:00:00Z",
        named: false,
        number: 1,
        summary: null,
      },
    ],
    ...overrides,
  };
}

function toolbar(): DocumentToolbar {
  return {
    connection: "connected",
    copyBlockLink: () => Promise.resolve(true),
    isNamingVersion: false,
    pending: undefined,
    requestNamedVersion: () => {},
    versions: [],
  };
}

function Location(): ReactNode {
  const { pathname, search } = useLocation();
  return <output data-testid="location">{`${pathname}${search}`}</output>;
}

/**
 * The document's own page, cut to the header: the artifact it draws comes from the issue's
 * document list read, which is the read the two controls mark stale, so a correction that lands
 * shows its new title here without the page being loaded again.
 */
function Page({ isClosed }: { readonly isClosed: boolean }): ReactNode {
  const read = useQuery({
    queryKey: ["artifacts", "CORE-1"],
    queryFn: () => api.listArtifacts("CORE-1"),
  });
  const artifact = read.data?.at(0);
  if (artifact === undefined) {
    return null;
  }
  return (
    <ArtifactHeader
      artifact={artifact}
      highlight={false}
      isClosed={isClosed}
      showVersionPicker={false}
      toolbar={toolbar()}
      version={undefined}
    >
      {null}
    </ArtifactHeader>
  );
}

/** Renders that page at `path`, with a document list of one that the writes below move. */
function renderPage(path: string, isClosed = false) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter initialEntries={[path]}>
      <QueryClientProvider client={queryClient}>
        <Page isClosed={isClosed} />
        <Location />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return view;
}

const documentPath = buildIssuePath({ key: "CORE-1", kind: "artifact", slug: "lauch-notes" });

// The correction writes the name alone and the slug does not move, so the person stays on the
// address they were on and the title they corrected is the one the page shows them.
test("the correction sends the rename and shows the new title with no reload", async () => {
  const user = userEvent.setup();
  let stored = document();
  const listArtifacts = spyOn(api, "listArtifacts").mockImplementation(() =>
    Promise.resolve([stored])
  );
  const renameArtifact = answerArtifactRenames().mockImplementation((_id, input) => {
    stored = { ...stored, name: input.name };
    return Promise.resolve(stored);
  });
  const view = renderPage(documentPath);

  try {
    await screen.findByRole("heading", { name: "Lauch notes" });
    await user.click(screen.getByRole("button", { name: "Correct title" }));
    const title = screen.getByRole("textbox", { name: "Title" });
    expect(title).toHaveValue("Lauch notes");
    await user.clear(title);
    await user.click(title);
    await user.paste("Launch notes");
    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() =>
      expect(renameArtifact).toHaveBeenCalledWith(
        "artifact-1",
        { name: "Launch notes" },
        keyedAttempt
      )
    );
    // The list read moved on its own, so the header and the issue's document list both show the
    // corrected title, and the address did not change.
    expect(await screen.findByRole("heading", { name: "Launch notes" })).not.toBeNull();
    expect(screen.getByTestId("location").textContent).toBe(documentPath);
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    renameArtifact.mockRestore();
  }
});

// Every name rule is the server's, so its refusal is shown as the sentence it came as.
test("a refused correction shows the server's own sentence", async () => {
  const user = userEvent.setup();
  const listArtifacts = spyOn(api, "listArtifacts").mockResolvedValue([document()]);
  const renameArtifact = answerArtifactRenames().mockRejectedValue(
    new ApiError(409, {
      code: "ARTIFACT_NAME_TAKEN",
      error: "CORE-1 already holds a document named Launch notes",
    })
  );
  const view = renderPage(documentPath);

  try {
    await screen.findByRole("heading", { name: "Lauch notes" });
    await user.click(screen.getByRole("button", { name: "Correct title" }));
    await user.clear(screen.getByRole("textbox", { name: "Title" }));
    await user.click(screen.getByRole("textbox", { name: "Title" }));
    await user.paste("Launch notes");
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "CORE-1 already holds a document named Launch notes"
    );
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    renameArtifact.mockRestore();
  }
});

// The removal leaves the person on the document's own address, which the issue no longer holds,
// so the artifacts panel's own branch says it is gone. A version route is normalised away: the
// version the person was reading went with the document.
test("the removal sends the delete and leaves the person on the gone document's address", async () => {
  const user = userEvent.setup();
  let stored: Artifact[] = [document()];
  const listArtifacts = spyOn(api, "listArtifacts").mockImplementation(() =>
    Promise.resolve(stored)
  );
  const deleteArtifact = answerArtifactRemovals().mockImplementation(() => {
    stored = [];
    return Promise.resolve();
  });
  const view = renderPage(`${documentPath}?v=1`);

  try {
    await screen.findByRole("heading", { name: "Lauch notes" });
    await user.click(screen.getByRole("button", { name: "Remove" }));

    await waitFor(() =>
      expect(deleteArtifact).toHaveBeenCalledWith("artifact-1", undefined, keyedAttempt)
    );
    await waitFor(() => expect(screen.getByTestId("location").textContent).toBe(documentPath));
    // The list read moved, and it no longer holds the document.
    await waitFor(() => expect(screen.queryByRole("heading", { name: "Lauch notes" })).toBeNull());
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    deleteArtifact.mockRestore();
  }
});

// Dispatch removes only a document that holds nothing, and its refusal names which test the
// document failed; that sentence is shown where the person pressed.
test("a refused removal shows the server's own sentence", async () => {
  const user = userEvent.setup();
  const listArtifacts = spyOn(api, "listArtifacts").mockResolvedValue([document()]);
  const deleteArtifact = answerArtifactRemovals().mockRejectedValue(
    new ApiError(409, {
      code: "ARTIFACT_NOT_UNTOUCHED",
      error:
        "Lauch notes is not untouched: it holds writing; a document is removed only while it holds nothing",
    })
  );
  const view = renderPage(documentPath);

  try {
    await screen.findByRole("heading", { name: "Lauch notes" });
    await user.click(screen.getByRole("button", { name: "Remove" }));

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Lauch notes is not untouched: it holds writing; a document is removed only while it holds nothing"
    );
    expect(screen.getByRole("heading", { name: "Lauch notes" })).not.toBeNull();
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    deleteArtifact.mockRestore();
  }
});

// A closed issue takes no correction and no removal, as it takes no upload: each control says
// why before it is pressed, and pressing it writes nothing.
test("a closed issue refuses both ways back on the document's own page", async () => {
  const user = userEvent.setup();
  const listArtifacts = spyOn(api, "listArtifacts").mockResolvedValue([document()]);
  const renameArtifact = answerArtifactRenames().mockResolvedValue(document());
  const deleteArtifact = answerArtifactRemovals().mockResolvedValue(undefined);
  const view = renderPage(documentPath, true);

  try {
    await screen.findByRole("heading", { name: "Lauch notes" });
    const correct = screen.getByRole("button", { name: "Correct title" });
    const remove = screen.getByRole("button", { name: "Remove" });
    expect(correct).toHaveAttribute("aria-disabled", "true");
    expect(correct).toHaveAttribute(
      "title",
      "This issue is closed. Reopen it to correct the title."
    );
    expect(remove).toHaveAttribute("aria-disabled", "true");
    expect(remove).toHaveAttribute(
      "title",
      "This issue is closed. Reopen it to remove this document."
    );

    await user.click(correct);
    expect(screen.queryByRole("form", { name: "Correct title" })).not.toBeInTheDocument();
    await user.click(remove);
    expect(renameArtifact).not.toHaveBeenCalled();
    expect(deleteArtifact).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    renameArtifact.mockRestore();
    deleteArtifact.mockRestore();
  }
});

// A project's document is not removed: the header that covers both pages offers the correction
// there and no removal at all.
test("a project document's page offers the correction and no removal", async () => {
  const projectDocument = document({
    issue_key: null,
    ref_key: "CORE/lauch-notes",
  });
  const listArtifacts = spyOn(api, "listArtifacts").mockResolvedValue([projectDocument]);
  const view = renderPage(documentPath);

  try {
    await screen.findByRole("heading", { name: "Lauch notes" });
    expect(screen.getByRole("button", { name: "Correct title" })).not.toBeNull();
    expect(screen.queryByRole("button", { name: "Remove" })).not.toBeInTheDocument();
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
  }
});

// The spec's name is how the spec is written whole, so the server refuses a correction of it
// whatever name the press carries: its header offers no correction, where an ordinary
// document's header offers one.
test("the issue's spec offers no correction and an ordinary document does", async () => {
  const spec = document({
    name: "spec.md",
    primary: true,
    ref_key: "CORE-1/spec",
    slug: "spec",
  });
  const listSpec = spyOn(api, "listArtifacts").mockResolvedValue([spec]);
  const specPage = renderPage(buildIssuePath({ key: "CORE-1", kind: "artifact", slug: "spec" }));

  try {
    await screen.findByRole("heading", { name: "spec.md" });
    expect(screen.queryByRole("button", { name: "Correct title" })).not.toBeInTheDocument();
  } finally {
    specPage.unmount();
    listSpec.mockRestore();
  }

  const listDocument = spyOn(api, "listArtifacts").mockResolvedValue([document()]);
  const documentPage = renderPage(documentPath);

  try {
    await screen.findByRole("heading", { name: "Lauch notes" });
    expect(screen.getByRole("button", { name: "Correct title" })).not.toBeNull();
  } finally {
    documentPage.unmount();
    listDocument.mockRestore();
  }
});
