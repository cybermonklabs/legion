import { expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { realClock, tick } from "../../__tests__/fake-clock";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError } from "../../api/client";
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

/** Renders that page at `path`, with a document list of one that the writes below move, and
 *  hands back its query client, which a case that stops a late write reaches as the shell's
 *  indicator does. */
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
  return { ...view, queryClient };
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
  const renameArtifact = spyOn(api, "renameArtifact").mockImplementation((_id, input) => {
    stored = { ...stored, name: input.name };
    return Promise.resolve(stored);
  });
  const view = renderPage(documentPath);

  try {
    await screen.findByRole("heading", { name: "Lauch notes" });
    await user.click(screen.getByRole("button", { name: "Correct title" }));
    const title = screen.getByRole("textbox", { name: "Title" });
    expect(title).toHaveValue("Lauch notes");
    await saveTitle(user, "Launch notes");

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

/** Types `title` over the open correction's field and saves it. */
async function saveTitle(user: UserEvent, title: string): Promise<void> {
  const field = screen.getByRole("textbox", { name: "Title" });
  await user.clear(field);
  await user.click(field);
  await user.paste(title);
  await user.click(screen.getByRole("button", { name: "Save" }));
}

// A correction the reader stopped belongs to the press that made it, never to the title it
// carried. Here the stopped correction to "Launch notes" had reached Dispatch, a correction to
// "Launch plan" landed after it, and the person corrects the title back to "Launch notes". Sent
// under the stopped press's key, that press would be answered with what the stopped one did -
// Dispatch makes no request twice under one key - and the title would stay "Launch plan" with
// the form closed as if it were saved. So it goes under a key of its own, and the title ends as
// the person last wrote it.
test("correcting the title back to one a stopped correction carried goes under its own key and lands", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  let stored = document();
  const listArtifacts = spyOn(api, "listArtifacts").mockImplementation(() =>
    Promise.resolve([stored])
  );
  // Dispatch as it keeps a key (`api/request_keys.go`): the first request under a key makes the
  // correction, and each later one under it is answered with that answer, nothing made again.
  // While the line is down no answer comes back, though the correction was made.
  const answers = new Map<string, Artifact>();
  let answering = false;
  const renameArtifact = spyOn(api, "renameArtifact").mockImplementation((_id, input, bounds) => {
    const key = bounds?.key ?? "";
    let answer = answers.get(key);
    if (answer === undefined) {
      stored = { ...stored, name: input.name };
      answer = stored;
      answers.set(key, answer);
    }
    return answering
      ? Promise.resolve(answer)
      : Promise.reject(
          new NetworkError("/api/v1/artifacts/artifact-1", new TypeError("Failed to fetch"))
        );
  });
  const view = renderPage(documentPath);
  view.queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });

  try {
    await screen.findByRole("heading", { name: "Lauch notes" });
    await user.click(screen.getByRole("button", { name: "Correct title" }));
    await saveTitle(user, "Launch notes");
    await stopTheLateWrite(view.queryClient);
    const stoppedKey = renameArtifact.mock.calls[0]?.[2]?.key;

    answering = true;
    await saveTitle(user, "Launch plan");
    expect(await screen.findByRole("heading", { name: "Launch plan" })).not.toBeNull();

    await user.click(screen.getByRole("button", { name: "Correct title" }));
    await saveTitle(user, "Launch notes");
    await tick(1_000);

    const back = renameArtifact.mock.calls.at(-1);
    expect(back?.[1]).toEqual({ name: "Launch notes" });
    expect(back?.[2]?.key).not.toBe(stoppedKey);
    expect(stored.name).toBe("Launch notes");
    expect(await screen.findByRole("heading", { name: "Launch notes" })).not.toBeNull();
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    renameArtifact.mockRestore();
    realClock();
  }
});

// Every name rule is the server's, so its refusal is shown as the sentence it came as.
test("a refused correction shows the server's own sentence", async () => {
  const user = userEvent.setup();
  const listArtifacts = spyOn(api, "listArtifacts").mockResolvedValue([document()]);
  const renameArtifact = spyOn(api, "renameArtifact").mockRejectedValue(
    new ApiError(409, {
      code: "ARTIFACT_NAME_TAKEN",
      error: "CORE-1 already holds a document named Launch notes",
    })
  );
  const view = renderPage(documentPath);

  try {
    await screen.findByRole("heading", { name: "Lauch notes" });
    await user.click(screen.getByRole("button", { name: "Correct title" }));
    await saveTitle(user, "Launch notes");

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
  const deleteArtifact = spyOn(api, "deleteArtifact").mockImplementation(() => {
    stored = [];
    return Promise.resolve();
  });
  const view = renderPage(`${documentPath}?v=1`);

  try {
    await screen.findByRole("heading", { name: "Lauch notes" });
    await user.click(screen.getByRole("button", { name: "Remove" }));

    await waitFor(() => expect(deleteArtifact).toHaveBeenCalledWith("artifact-1", keyedAttempt));
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
  const deleteArtifact = spyOn(api, "deleteArtifact").mockRejectedValue(
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

// A removal the reader stopped may have reached Dispatch. Pressed while its refusal is on screen,
// Remove is the Retry of that removal (`retry`), so it goes under the stopped removal's key and
// Dispatch answers with what it did there: the document is gone, and the person is not told
// Dispatch found no document to remove.
test("Remove pressed again after a stopped removal makes that removal again under its key", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  let stored: Artifact[] = [document()];
  const listArtifacts = spyOn(api, "listArtifacts").mockImplementation(() =>
    Promise.resolve(stored)
  );
  // Dispatch as it keeps a key: the first request under a key removes the document, a later one
  // under it is answered as that one was, and a removal under another key finds no document.
  // While the line is down no answer comes back, though the document was removed.
  const removedUnder = new Set<string>();
  let answering = false;
  const deleteArtifact = spyOn(api, "deleteArtifact").mockImplementation((_id, bounds) => {
    const key = bounds?.key ?? "";
    if (!removedUnder.has(key)) {
      if (stored.length === 0) {
        return Promise.reject(
          new ApiError(404, { code: "ARTIFACT_NOT_FOUND", error: "artifact not found" })
        );
      }
      stored = [];
      removedUnder.add(key);
    }
    return answering
      ? Promise.resolve()
      : Promise.reject(
          new NetworkError("/api/v1/artifacts/artifact-1", new TypeError("Failed to fetch"))
        );
  });
  const view = renderPage(documentPath);
  view.queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });

  try {
    await screen.findByRole("heading", { name: "Lauch notes" });
    await user.click(screen.getByRole("button", { name: "Remove" }));
    await stopTheLateWrite(view.queryClient);
    expect(screen.getByRole("alert")).not.toBeNull();
    const stoppedKey = deleteArtifact.mock.calls[0]?.[1]?.key;
    const stopped = deleteArtifact.mock.calls.length;

    answering = true;
    await user.click(screen.getByRole("button", { name: "Remove" }));
    await tick(1_000);

    expect(deleteArtifact).toHaveBeenCalledTimes(stopped + 1);
    expect(deleteArtifact.mock.calls.at(-1)?.[1]?.key).toBe(stoppedKey);
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.queryByRole("heading", { name: "Lauch notes" })).toBeNull();
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    deleteArtifact.mockRestore();
    realClock();
  }
});

// A closed issue takes no correction and no removal, as it takes no upload: each control says
// why before it is pressed, and pressing it writes nothing.
test("a closed issue refuses both ways back on the document's own page", async () => {
  const user = userEvent.setup();
  const listArtifacts = spyOn(api, "listArtifacts").mockResolvedValue([document()]);
  const renameArtifact = spyOn(api, "renameArtifact").mockResolvedValue(document());
  const deleteArtifact = spyOn(api, "deleteArtifact").mockResolvedValue(undefined);
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
