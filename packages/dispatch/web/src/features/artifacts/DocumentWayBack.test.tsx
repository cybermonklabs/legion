import { beforeAll, expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import type { ReactNode } from "react";
import { Link, MemoryRouter, useLocation } from "react-router-dom";

import { realClock, tick } from "../../__tests__/fake-clock";
import { graphNode } from "../../__tests__/graph-fixture";
import { heldNotDisabled } from "../../__tests__/held-press";
import { heldUntilLate, pressesOf } from "../../__tests__/held-until-late";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError, REQUEST_DEADLINE_MS } from "../../api/client";
import type { Artifact } from "../../api/types";
import type { DocumentToolbar } from "../doc/ProofDocument";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { buildIssuePath, parseIssuePath } from "../refs/routes";
import { SEND_DEADLINE_MS, SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { ArtifactHeader } from "./ArtifactHeader";
import { ArtifactRoutePanel } from "./ArtifactRoutePanel";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

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

/** The address of `artifact`'s own page on its issue. */
function pathOf(artifact: Artifact): string {
  return buildIssuePath({ key: "CORE-1", kind: "artifact", slug: artifact.slug });
}

/**
 * The document's own page, cut to the header and a link to each of the issue's documents: as on
 * the issue's page, the header draws the document the route names, and one header is drawn for
 * one document after another as the route moves. The artifact comes from the issue's document
 * list read, which is the read the two controls mark stale, so a correction that lands shows its
 * new title here without the page being loaded again.
 */
function Page({ isClosed }: { readonly isClosed: boolean }): ReactNode {
  const { pathname } = useLocation();
  const route = parseIssuePath(pathname);
  const read = useQuery({
    queryKey: ["artifacts", "CORE-1"],
    queryFn: () => api.listArtifacts("CORE-1"),
  });
  const artifact =
    route?.kind === "artifact" ? read.data?.find(({ slug }) => slug === route.slug) : undefined;
  return (
    <>
      <nav>
        {read.data?.map((each) => (
          <Link key={each.id} to={pathOf(each)}>
            {each.name}
          </Link>
        ))}
      </nav>
      {artifact === undefined ? null : (
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
      )}
    </>
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

test.each([
  { name: "Save", label: "Rename Lauch notes to Launch notes" },
  { name: "Remove", label: "Remove Lauch notes" },
] as const)(
  "$name lets go at the document write deadline and sends again",
  async ({ label, name }) => {
    const listArtifacts = spyOn(api, "listArtifacts").mockResolvedValue([document()]);
    const dropped = new NetworkError("/api/v1/artifacts", new TypeError("Failed to fetch"));
    const rename = spyOn(api, "renameArtifact").mockRejectedValue(dropped);
    const remove = spyOn(api, "deleteArtifact").mockRejectedValue(dropped);
    const view = renderPage(documentPath);
    try {
      await screen.findByRole("heading", { name: "Lauch notes" });
      let cancel: HTMLElement | undefined;
      if (name === "Save") {
        const prepare = userEvent.setup();
        await prepare.click(screen.getByRole("button", { name: "Correct title" }));
        await prepare.clear(screen.getByRole("textbox", { name: "Title" }));
        await prepare.paste("Launch notes");
        cancel = screen.getByRole("button", { name: "Cancel" });
      }
      const button = screen.getByRole<HTMLButtonElement>("button", { name });
      jest.useFakeTimers();
      const user = userOnFakeClock();
      const sent = () => pressesOf([rename, remove]);
      await heldUntilLate(user, button, {
        alongside: (held) => {
          if (cancel !== undefined) {
            expect(cancel.getAttribute("aria-disabled"), "Cancel").toBe(held ? "true" : null);
          }
        },
        label,
        name,
        queryClient: view.queryClient,
        sent,
      });
      await user.keyboard("{Enter}");
      await tick(1_000);
      expect(sent()).toBe(2);
    } finally {
      realClock();
      view.unmount();
      view.queryClient.clear();
      listArtifacts.mockRestore();
      rename.mockRestore();
      remove.mockRestore();
    }
  }
);

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

/**
 * Dispatch's answers, held until the case gives them: a mocked write returns `answer(reply)`,
 * whose `reply` returns the answer or throws the refusal, and `give()` gives every answer held
 * since its last call inside `act`, once the press that sent it is out, as docs/testing.md
 * ("The act rule of a Dispatch unit test") says. On the held clock a press's own wait ends a
 * few turns after its last event, so an answer given at once lands after that wait and updates
 * the page outside `act`.
 */
function heldAnswers() {
  let held = Promise.withResolvers<void>();
  return {
    answer<T>(reply: () => T): Promise<T> {
      return held.promise.then(reply);
    },
    async give(): Promise<void> {
      const given = held;
      held = Promise.withResolvers<void>();
      await act(async () => {
        given.resolve();
      });
    },
  };
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
  // While the line is down no answer comes back, though the correction was made; once it is up,
  // each answer is held for the case to give (`heldAnswers`).
  const answers = new Map<string, Artifact>();
  const held = heldAnswers();
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
      ? held.answer(() => answer)
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
    await held.give();
    expect(await screen.findByRole("heading", { name: "Launch plan" })).not.toBeNull();

    await user.click(screen.getByRole("button", { name: "Correct title" }));
    await saveTitle(user, "Launch notes");
    await held.give();
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

// The heading of a page whose correction the reader stopped still shows the title the page read
// before the press, which may no longer be the title: here the correction had reached Dispatch.
// So the page reads the document again, and the heading shows the title Dispatch holds, which is
// what the form's sentence sends the reader to read before pressing Save again.
test("a stopped correction reads the document again, so the heading shows the title Dispatch holds", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  let stored = document();
  const listArtifacts = spyOn(api, "listArtifacts").mockImplementation(() =>
    Promise.resolve([stored])
  );
  const renameArtifact = spyOn(api, "renameArtifact").mockImplementation((_id, input) => {
    stored = { ...stored, name: input.name };
    return Promise.reject(
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

    expect(stored.name).toBe("Launch notes");
    expect(screen.getByRole("heading", { name: "Launch notes" })).not.toBeNull();
    const said = screen
      .getByRole("form", { name: "Correct title" })
      .querySelector('[role="alert"]');
    expect(said?.textContent).toBe(
      "Dispatch has not confirmed the title “Launch notes”: it may have been saved. The heading " +
        "reads the title again from Dispatch: where it does not show this one, Save sends it " +
        "again as a new correction."
    );
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

// INFRA-3135. The focus follows the form: Correct title opens it with the focus in Title, and
// Cancel closes it with the focus back on Correct title. By the keyboard the same: Enter on the
// button opens it, and Enter in Title with the title as it stands closes it, the focus again on
// the button that opened it rather than dropped on the page.
test("Correct title puts the focus in Title, and closing the form gives it back to the button", async () => {
  const user = userEvent.setup();
  const listArtifacts = spyOn(api, "listArtifacts").mockResolvedValue([document()]);
  const view = renderPage(documentPath);

  try {
    await screen.findByRole("heading", { name: "Lauch notes" });
    await user.click(screen.getByRole("button", { name: "Correct title" }));
    expect(screen.getByRole("textbox", { name: "Title" })).toHaveFocus();
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.getByRole("button", { name: "Correct title" })).toHaveFocus();

    await user.keyboard("{Enter}");
    expect(screen.getByRole("textbox", { name: "Title" })).toHaveFocus();
    await user.keyboard("{Enter}");
    expect(screen.getByRole("button", { name: "Correct title" })).toHaveFocus();
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
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

// A removal is a `DELETE`, which sends no key, and Dispatch keeps no answer for one. Here the
// reader stopped a removal that had reached Dispatch while the line was down: the document is
// gone, and no answer came back. Pressed while its refusal is on screen, Remove is the Retry of
// that removal (`retry`), a new `DELETE` to Dispatch, which finds no document and answers
// `404 ARTIFACT_NOT_FOUND`. That is the removal done - the document is gone, which is what the
// person asked - so the page shows the document removed and no refusal.
test("Remove after a stopped removal that landed takes the not-found as done", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  let stored: Artifact[] = [document()];
  const listArtifacts = spyOn(api, "listArtifacts").mockImplementation(() =>
    Promise.resolve(stored)
  );
  // Dispatch as it answers a `DELETE`, whatever key the page hands the call: one that finds the
  // document removes it, and one after it finds none. While the line is down no answer comes
  // back, though the first removal reached Dispatch and was made; once it is up, each answer is
  // held for the case to give (`heldAnswers`).
  const held = heldAnswers();
  let answering = false;
  const deleteArtifact = spyOn(api, "deleteArtifact").mockImplementation(() => {
    if (!answering) {
      stored = [];
      return Promise.reject(
        new NetworkError("/api/v1/artifacts/artifact-1", new TypeError("Failed to fetch"))
      );
    }
    if (stored.length === 0) {
      return held.answer(() => {
        throw new ApiError(404, { code: "ARTIFACT_NOT_FOUND", error: "artifact not found" });
      });
    }
    stored = [];
    return held.answer(() => undefined);
  });
  const view = renderPage(documentPath);
  view.queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });

  try {
    await screen.findByRole("heading", { name: "Lauch notes" });
    await user.click(screen.getByRole("button", { name: "Remove" }));
    await stopTheLateWrite(view.queryClient);
    expect(screen.getByRole("alert")).not.toBeNull();
    // The key each call is handed is the one the page holds that press under, which a `DELETE`
    // does not send: what it tells here is that Remove made the stopped press again.
    const stoppedKey = deleteArtifact.mock.calls[0]?.[1]?.key;
    const stopped = deleteArtifact.mock.calls.length;

    answering = true;
    await user.click(screen.getByRole("button", { name: "Remove" }));
    await held.give();
    await tick(1_000);

    expect(deleteArtifact).toHaveBeenCalledTimes(stopped + 1);
    expect(deleteArtifact.mock.calls.at(-1)?.[1]?.key).toBe(stoppedKey);
    expect(screen.queryByRole("alert")).toBeNull();
    // The removal landed, so the page read the document list again, which no longer holds it.
    expect(screen.queryByRole("heading", { name: "Lauch notes" })).toBeNull();
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    deleteArtifact.mockRestore();
    realClock();
  }
});

/** A second document of the issue, which the reader moves to from the first. */
const releaseNotes = document({
  id: "artifact-2",
  name: "Release notes",
  ref_key: "CORE-1/release-notes",
  slug: "release-notes",
});

// One header is drawn for one document after another as the route moves, and a refusal belongs
// to the document whose removal it refused. The reader who moves on from that document is shown
// none of it, and Remove there removes the document now shown: a Retry of the refused removal
// would ask for it again, refused the same way each time, and this document could not be removed.
test("after a refused removal, the next document's page shows no refusal and its Remove removes it", async () => {
  const user = userEvent.setup();
  let stored: Artifact[] = [document(), releaseNotes];
  const listArtifacts = spyOn(api, "listArtifacts").mockImplementation(() =>
    Promise.resolve(stored)
  );
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
    expect(await screen.findByRole("alert")).toHaveTextContent("Lauch notes is not untouched");

    await user.click(screen.getByRole("link", { name: "Release notes" }));
    await screen.findByRole("heading", { name: "Release notes" });
    expect(screen.queryByRole("alert")).toBeNull();

    deleteArtifact.mockImplementation(() => {
      stored = [document()];
      return Promise.resolve();
    });
    await user.click(screen.getByRole("button", { name: "Remove" }));

    await waitFor(() => expect(deleteArtifact).toHaveBeenCalledTimes(2));
    expect(deleteArtifact).toHaveBeenLastCalledWith("artifact-2", keyedAttempt);
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    deleteArtifact.mockRestore();
  }
});

// A removal lands when Dispatch answers it, by when the reader may have moved to another
// document. The way back to the gone document's address is for a reader still on its page: one
// who moved on stays on the page they moved to, which is still there.
test("a removal that lands after the reader moved to another document leaves them on that one", async () => {
  const user = userEvent.setup();
  let stored: Artifact[] = [document(), releaseNotes];
  const listArtifacts = spyOn(api, "listArtifacts").mockImplementation(() =>
    Promise.resolve(stored)
  );
  const { promise: landed, resolve: land } = Promise.withResolvers<void>();
  const deleteArtifact = spyOn(api, "deleteArtifact").mockImplementation(async () => {
    await landed;
    stored = [releaseNotes];
  });
  const view = renderPage(documentPath);

  try {
    await screen.findByRole("heading", { name: "Lauch notes" });
    await user.click(screen.getByRole("button", { name: "Remove" }));
    await waitFor(() => expect(deleteArtifact).toHaveBeenCalledWith("artifact-1", keyedAttempt));

    await user.click(screen.getByRole("link", { name: "Release notes" }));
    await screen.findByRole("heading", { name: "Release notes" });
    await act(async () => land());
    await waitFor(() => expect(view.queryClient.isMutating()).toBe(0));

    expect(screen.getByTestId("location").textContent).toBe(pathOf(releaseNotes));
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    deleteArtifact.mockRestore();
  }
});

// The header keys its Remove by the document (`ArtifactHeader`), so a reader who moves to another
// document while a removal is still out, before it lands, and comes back meets a Remove drawn
// again. It holds from that removal's own state (`usePendingKeyedPress`), as the one pressed did:
// `Removing…`, a press of it sends nothing more, and it lets go at the removal's deadline
// (`SEND_DEADLINE_MS`). Dispatch never answers it: each try fails at the network and is tried
// again under its key.
test("a document's Remove drawn again while its removal is out holds until the removal's deadline and sends nothing more", async () => {
  const listArtifacts = spyOn(api, "listArtifacts").mockResolvedValue([document(), releaseNotes]);
  const remove = spyOn(api, "deleteArtifact").mockRejectedValue(
    new NetworkError("/api/v1/artifacts", new TypeError("Failed to fetch"))
  );
  const view = renderPage(documentPath);
  const sent = () => pressesOf([remove]);
  try {
    await screen.findByRole("heading", { name: "Lauch notes" });
    jest.useFakeTimers();
    const user = userOnFakeClock();
    await user.click(screen.getByRole("button", { name: "Remove" }));
    await tick(0);
    expect(sent()).toBe(1);

    await user.click(screen.getByRole("link", { name: "Release notes" }));
    await tick(0);
    expect(screen.getByRole("heading", { name: "Release notes" })).toBeInTheDocument();
    await user.click(screen.getByRole("link", { name: "Lauch notes" }));
    await tick(0);
    const button = within(screen.getByTestId("artifact-header")).getByRole<HTMLButtonElement>(
      "button",
      { name: /^(Remove|Removing…)$/ }
    );
    expect(button.textContent, "the Remove drawn again let go while its removal was out").toBe(
      "Removing…"
    );
    expect(heldNotDisabled(button, "Remove")).toBe(true);
    await user.click(button);
    await tick(0);
    expect(sent(), "a press of the Remove drawn again sent a second removal").toBe(1);

    await tick(SEND_DEADLINE_MS - 1_000);
    expect(heldNotDisabled(button, "Remove")).toBe(true);
    await tick(1_000);
    expect(heldNotDisabled(button, "Remove")).toBe(false);
    expect(button.textContent).toBe("Remove");
  } finally {
    realClock();
    view.unmount();
    view.queryClient.clear();
    listArtifacts.mockRestore();
    remove.mockRestore();
  }
});

// A correction belongs to the document it was made on, as a removal does: the open form, the
// title typed into it and what became of its Save. Here Dispatch had not answered it when the
// reader stopped it. The reader who moves on to the next document is shown none of it, and Save
// there corrects the document now shown, to the title typed for it: the form left open would
// rename this document to the other's title, the heading under it already this one's.
test("after a stopped correction, the next document's page shows none of it and Save renames that document", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  let stored: Artifact[] = [document(), releaseNotes];
  const listArtifacts = spyOn(api, "listArtifacts").mockImplementation(() =>
    Promise.resolve(stored)
  );
  // Once the line is up, each answer is held for the case to give (`heldAnswers`).
  const held = heldAnswers();
  let answering = false;
  const renameArtifact = spyOn(api, "renameArtifact").mockImplementation((id, input) => {
    if (!answering) {
      return Promise.reject(
        new NetworkError(`/api/v1/artifacts/${id}`, new TypeError("Failed to fetch"))
      );
    }
    stored = stored.map((each) => (each.id === id ? { ...each, name: input.name } : each));
    const renamed = stored.find((each) => each.id === id) ?? releaseNotes;
    return held.answer(() => renamed);
  });
  const view = renderPage(documentPath);
  view.queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });

  try {
    await screen.findByRole("heading", { name: "Lauch notes" });
    await user.click(screen.getByRole("button", { name: "Correct title" }));
    await saveTitle(user, "Launch notes");
    await stopTheLateWrite(view.queryClient);
    expect(screen.getByRole("alert")).toHaveTextContent(
      "Dispatch has not confirmed the title “Launch notes”"
    );

    await user.click(screen.getByRole("link", { name: "Release notes" }));
    await screen.findByRole("heading", { name: "Release notes" });
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.queryByRole("form", { name: "Correct title" })).toBeNull();

    answering = true;
    await user.click(screen.getByRole("button", { name: "Correct title" }));
    expect(screen.getByRole("textbox", { name: "Title" })).toHaveValue("Release notes");
    await saveTitle(user, "Release plan");
    await held.give();
    await tick(1_000);

    expect(renameArtifact).toHaveBeenLastCalledWith(
      "artifact-2",
      { name: "Release plan" },
      keyedAttempt
    );
    expect(renameArtifact).not.toHaveBeenCalledWith(
      "artifact-2",
      { name: "Launch notes" },
      keyedAttempt
    );
    expect(screen.getByRole("heading", { name: "Release plan" })).not.toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    renameArtifact.mockRestore();
    realClock();
  }
});

// A removal the reader stopped may have been made, so the row does not say it failed. Remove is
// its Retry (`retry`), so the row says what a press of Remove does with the stopped removal.
test("a removal the reader stopped says Remove makes it again, not that it failed", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const listArtifacts = spyOn(api, "listArtifacts").mockResolvedValue([document()]);
  const deleteArtifact = spyOn(api, "deleteArtifact").mockRejectedValue(
    new NetworkError("/api/v1/artifacts/artifact-1", new TypeError("Failed to fetch"))
  );
  const view = renderPage(documentPath);
  view.queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });

  try {
    await screen.findByRole("heading", { name: "Lauch notes" });
    await user.click(screen.getByRole("button", { name: "Remove" }));
    await stopTheLateWrite(view.queryClient);

    expect(screen.getByRole("alert").textContent).toBe(
      stoppedWriteNote("the removal of “Lauch notes”", "made", {
        again: "Remove",
        method: "DELETE",
        stopped: new SendStoppedError(),
      })
    );
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

/**
 * The issue's page cut to its documents' panel (`ArtifactRoutePanel`), as the issue's page draws
 * it: the document the route names comes from the issue read, as `useIssueDetail` takes it, so
 * nothing on a document's own page reads the issue's document list, which only the Artifacts tab
 * draws. A removal's way back - the document's address, which says the document is gone - and its
 * `View artifacts` link are the panel's own.
 */
function IssuePage(): ReactNode {
  const { pathname } = useLocation();
  const route = parseIssuePath(pathname);
  const issue = useQuery({ queryKey: ["issue", "CORE-1"], queryFn: () => api.getIssue("CORE-1") });
  if (route === undefined || issue.data === undefined) return null;
  const artifactRoute = route.kind === "artifact" ? route : undefined;
  return (
    <ArtifactRoutePanel
      active
      artifact={issue.data.artifacts?.find(({ slug }) => slug === artifactRoute?.slug)}
      artifactRoute={artifactRoute}
      isClosed={false}
      isPrimaryArtifactRoute={false}
      mounted
      onShowDiffChange={() => {}}
      route={route}
      showDiff={false}
      toolbar={toolbar()}
    >
      {null}
    </ArtifactRoutePanel>
  );
}

/** A read of the issue's page a row holds from `hold()` on, each answered once the row calls
 *  `answer()`, with what the page holds by then. */
function heldRead<T>(read: () => T): {
  readonly answer: () => void;
  readonly hold: () => void;
  readonly next: () => Promise<T>;
} {
  let holding = false;
  const waiting: (() => void)[] = [];
  return {
    answer: () => {
      holding = false;
      for (const give of waiting.splice(0)) give();
    },
    hold: () => {
      holding = true;
    },
    next: () => {
      if (!holding) return Promise.resolve(read());
      const { promise, resolve } = Promise.withResolvers<T>();
      waiting.push(() => resolve(read()));
      return promise;
    },
  };
}

/** The issue's page at `path` (`IssuePage`), over an issue that holds `stored()`'s documents,
 *  with the issue read and the document list read handed out by `issue` and `list`. */
function renderIssuePage(
  path: string,
  stored: () => Artifact[],
  { issue, list }: { issue: () => Promise<unknown>; list: () => Promise<Artifact[]> }
) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const mocks = [
    spyOn(api, "getIssue").mockImplementation(
      () => issue().then(() => ({ artifacts: stored(), key: "CORE-1" })) as never
    ),
    spyOn(api, "listArtifacts").mockImplementation(list),
    spyOn(api, "getIssueReferences").mockResolvedValue({
      linked_from: [],
      links: [],
      members: [],
      truncated: false,
    }),
    // The document's Referenced by panel (`ArtifactDetails`) reads its edges.
    spyOn(api, "getReferences").mockResolvedValue({
      edges: [],
      node: graphNode({ id: "artifact-1", kind: "artifact" }),
    }),
  ];
  const view = render(
    <MemoryRouter initialEntries={[path]}>
      <QueryClientProvider client={queryClient}>
        <IssuePage />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    queryClient,
    unmount: () => {
      view.unmount();
      queryClient.clear();
      for (const mock of mocks) mock.mockRestore();
    },
  };
}

/** What a removal says once the reads after it have outlived the request deadline. */
const ARTIFACTS_UNREAD = "Could not load artifacts. Dispatch did not answer within 30 seconds.";

// A document's own page draws the document from the issue read and reads no document list, so
// its Remove holds through the issue read, which takes the document off the page
// (`refreshRemoval`): held until that read answers, and beside Remove, once the read has outlived
// the request deadline, the header says why, until the read answers and the page says the
// document is gone (the tester's row of `document-way-back.e2e.ts`).
test("a Remove on a document's own page holds through the issue read and says why beside Remove at the bound until that read answers", async () => {
  let stored = [document()];
  const issueRead = heldRead(() => undefined);
  const deleteArtifact = spyOn(api, "deleteArtifact").mockImplementation(async () => {
    stored = [];
  });
  const page = renderIssuePage(documentPath, () => stored, {
    issue: issueRead.next,
    list: () => Promise.resolve(stored),
  });
  try {
    await screen.findByRole("heading", { name: "Lauch notes" });
    const header = () => screen.getByTestId("artifact-header");
    const remove = () => within(header()).getByRole("button", { name: /^(Remove|Removing…)$/ });
    jest.useFakeTimers();
    issueRead.hold();
    await userOnFakeClock().click(remove());
    await tick(0);
    expect(deleteArtifact).toHaveBeenCalledTimes(1);
    expect(remove().textContent, "Remove let go before the issue read answered").toBe("Removing…");

    await tick(REQUEST_DEADLINE_MS);
    expect(remove().textContent).toBe("Remove");
    expect(
      within(header()).queryByRole("alert")?.textContent,
      "the header said nothing of the issue read the removal made"
    ).toBe(ARTIFACTS_UNREAD);

    await act(async () => issueRead.answer());
    await tick(0);
    expect(screen.queryByTestId("artifact-header")).toBeNull();
    expect(screen.getByText("No artifact lauch-notes on CORE-1.")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).toBeNull();
  } finally {
    realClock();
    page.unmount();
    deleteArtifact.mockRestore();
  }
});

// The removal's way back keeps the reader on the document's address, which says it is gone, and
// its `View artifacts` opens the Artifacts tab. The removal reads the issue's document list even
// though nothing on the document's page draws it (`refreshRemoval`), so the tab finds that read
// out and the removal's hold through it: it lists the removed document while the read is out, and
// says why on its row once the read has outlived the request deadline, until the read answers and
// takes the row away (the tester's second row of `document-way-back.e2e.ts`).
test("the Artifacts tab a document page's Remove opens on its way back says why on the removed row while the list read the removal made is out", async () => {
  let stored = [document()];
  const listRead = heldRead(() => stored);
  const deleteArtifact = spyOn(api, "deleteArtifact").mockImplementation(async () => {
    stored = [];
  });
  // The reader comes to the document from the Artifacts tab, which reads the issue's documents.
  const page = renderIssuePage(buildIssuePath({ key: "CORE-1", kind: "artifacts" }), () => stored, {
    issue: () => Promise.resolve(),
    list: listRead.next,
  });
  try {
    const user = userEvent.setup();
    await user.click(
      within(await screen.findByTestId("artifact-lauch-notes")).getByRole("link", {
        name: "Lauch notes",
      })
    );
    await screen.findByRole("heading", { name: "Lauch notes" });
    jest.useFakeTimers();
    const clockUser = userOnFakeClock();
    listRead.hold();
    await clockUser.click(screen.getByRole("button", { name: "Remove" }));
    await tick(0);
    expect(deleteArtifact).toHaveBeenCalledTimes(1);
    expect(screen.getByText("No artifact lauch-notes on CORE-1.")).toBeInTheDocument();

    await clockUser.click(screen.getByRole("link", { name: "View artifacts" }));
    await tick(0);
    const row = () => screen.getByTestId("artifact-lauch-notes");
    expect(row(), "the tab lists the removed document while its list read is out").not.toBeNull();
    await tick(REQUEST_DEADLINE_MS);
    expect(
      within(row()).queryByRole("alert")?.textContent,
      "the tab said nothing of the list read the removal made"
    ).toBe(ARTIFACTS_UNREAD);

    await act(async () => listRead.answer());
    await tick(0);
    expect(screen.queryByTestId("artifact-lauch-notes")).toBeNull();
  } finally {
    realClock();
    page.unmount();
    deleteArtifact.mockRestore();
  }
});
