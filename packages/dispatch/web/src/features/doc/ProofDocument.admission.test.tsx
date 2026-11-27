import { beforeAll, beforeEach, expect, jest, spyOn, test } from "bun:test";
import type { ArtifactRebuildReport } from "@legion/contracts";
import { onlineManager } from "@tanstack/react-query";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import manifest from "../../../../../envoy/internal/dispatch/pmdoc/schema/blocks.json";
import { fakeDocumentRuntime } from "../../__tests__/document-runtime";
import { realClock, tick } from "../../__tests__/fake-clock";
import { heldAnswers } from "../../__tests__/held-answers";
import { heldNotDisabled, pressHeld } from "../../__tests__/held-press";
import { heldUntilLate, pressesOf } from "../../__tests__/held-until-late";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { answerRequests } from "../../__tests__/network";
import {
  answerTextReadsWithTheSeededText,
  artifact,
  createQueryClient,
  documentSurfaceHandlers,
  flushLoadFailure,
  renderProofDocument,
} from "../../__tests__/proof-document";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError, REQUEST_DEADLINE_MS } from "../../api/client";
import { DispatchQueryClient } from "../../api/query-client";
import type { ArtifactText } from "../../api/types";
import { emulateChromiumFocusFixup } from "../issue-page/chromium-focus-fixup";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { SEND_DEADLINE_MS, stoppedWriteNote } from "../writes/keyed-write";
import { deletePendingEdits, openPendingEdits } from "./pending-edits";
import { compiledSchemaCreatable } from "./schema";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

// Whether the live editor may connect, and what the page shows when it may not or cannot: the
// admission's own `/text` read, a socket the server refuses as outside the Proof schema, the repair
// and rebuild it offers, and a transport or editor that fails to load.

// The messages the server answers `409 DOC_SCHEMA` and `409 DOCUMENT_UNLOADABLE` with
// (`docs.ErrDocOutsideSchema`, `docs.ErrDocumentUnloadable`).
const SCHEMA_REFUSAL =
  "document is outside the Proof schema. A replacement from markdown discards every part of the document's current content that the markdown leaves out. A person decides whether to replace the document";
const UNLOADABLE_REFUSAL =
  "the document's stored history cannot load. A rebuild from the latest saved version discards every edit made after that version. A person decides whether to rebuild the document";

async function pendingEditCount(artifactId: string): Promise<number> {
  const store = await openPendingEdits(artifactId, -1);
  if (store === undefined) {
    throw new Error("fake-indexeddb refused to open");
  }
  try {
    return await store.count();
  } finally {
    store.close();
  }
}

answerTextReadsWithTheSeededText();

beforeEach(() => {
  answerRequests(...documentSurfaceHandlers());
});

// The repair replaces this document, so it uploads under the document's own name whatever the
// picked file is called (a second `spec (1).md` would leave `spec.md` unreadable), and the read
// that follows the upload admits the editor again.
test("ProofDocument repairs a document outside the schema by an upload under its own name", async () => {
  const user = userEvent.setup();
  const getArtifactText = spyOn(api, "getArtifactText").mockRejectedValue(
    new ApiError(409, { code: "DOC_SCHEMA", error: SCHEMA_REFUSAL })
  );
  const uploadArtifact = spyOn(api, "uploadArtifact").mockResolvedValue({
    artifact,
    version: {
      authors: [{ id: "alice", kind: "user" }],
      created_at: "2026-10-02T00:00:00Z",
      named: true,
      number: 2,
      summary: null,
    },
  });
  try {
    const { connections, view } = renderProofDocument();

    expect(await screen.findByText(SCHEMA_REFUSAL)).not.toBeNull();
    expect(connections).toHaveLength(0);
    const picked = new File(["repaired\n"], "spec (1).md", { type: "text/markdown" });
    await user.upload(screen.getByLabelText("Upload artifact"), picked);
    getArtifactText.mockResolvedValue({
      markdown: "repaired\n",
      token: "sha256:repaired",
      version: 2,
    });
    await user.click(await screen.findByRole("button", { name: "Upload" }));
    await waitFor(() => expect(uploadArtifact).toHaveBeenCalledTimes(1));
    expect(uploadArtifact.mock.calls[0]).toEqual([
      { issue: "CORE-1" },
      { file: picked, name: "spec.md", summary: undefined },
      keyedAttempt,
    ]);
    await waitFor(() => expect(connections).toHaveLength(1));
    expect(
      screen.queryByText("Upload markdown to replace and repair this document.")
    ).not.toBeInTheDocument();
    view.unmount();
  } finally {
    getArtifactText.mockRestore();
    uploadArtifact.mockRestore();
  }
});

test("ProofDocument removes locally saved edits only when its artifact is gone", async () => {
  const stored = await openPendingEdits(artifact.id, 1);
  if (stored === undefined) {
    throw new Error("fake-indexeddb refused to open");
  }
  await stored.record(Uint8Array.of(0)).written;
  stored.close();
  const getArtifactText = spyOn(api, "getArtifactText").mockRejectedValue(
    new ApiError(404, { code: "ARTIFACT_NOT_FOUND", error: "artifact not found" })
  );
  try {
    const { view } = renderProofDocument();
    await screen.findByText("This document could not load: artifact not found");
    await waitFor(async () => expect(await pendingEditCount(artifact.id)).toBe(0));
    view.unmount();
  } finally {
    getArtifactText.mockRestore();
  }
});

test("ProofDocument keeps locally saved edits when another 404 blocks admission", async () => {
  const document = { ...artifact, id: "artifact-404-keeps-edits" };
  const stored = await openPendingEdits(document.id, 1);
  if (stored === undefined) {
    throw new Error("fake-indexeddb refused to open");
  }
  await stored.record(Uint8Array.of(0)).written;
  stored.close();
  const getArtifactText = spyOn(api, "getArtifactText").mockRejectedValue(
    new ApiError(404, { code: "PROXY_NOT_FOUND", error: "proxy not found" })
  );
  try {
    const { view } = renderProofDocument({ document });
    await screen.findByText("This document could not load: proxy not found");
    expect(await pendingEditCount(document.id)).toBe(1);
    view.unmount();
  } finally {
    getArtifactText.mockRestore();
    // The edit this row kept stays in the process's one IndexedDB, where a rerun of this file
    // would count it beside its own.
    await deletePendingEdits(document.id);
  }
});

test("ProofDocument waits for a fresh text read before reconnecting from cached text", async () => {
  const queryClient = createQueryClient();
  queryClient.setQueryData(["artifact", artifact.id, "text"], {
    markdown: "cached document",
    version: null,
  });
  let resolveText: (value: ArtifactText) => void;
  const textRead = new Promise<ArtifactText>((resolve) => {
    resolveText = resolve;
  });
  const getArtifactText = spyOn(api, "getArtifactText").mockImplementation(() => textRead);
  try {
    const { connections, view } = renderProofDocument({ queryClient });

    await waitFor(() => expect(getArtifactText).toHaveBeenCalledTimes(1));
    expect(connections).toHaveLength(0);

    await act(async () => {
      resolveText({ markdown: "fresh document", token: "sha256:fresh-document", version: null });
      await textRead;
    });
    await waitFor(() => expect(connections).toHaveLength(1));
    view.unmount();
  } finally {
    getArtifactText.mockRestore();
  }
});

// An artifact event invalidates the document's queries, its text read among them, every few
// seconds while anyone edits. The fresh read gates admission only: a refetch after the editor
// connects leaves that editor and its socket alone.
test("ProofDocument keeps its connected editor through a later text refetch", async () => {
  const queryClient = createQueryClient();
  const { connections, editors, sync, view } = renderProofDocument({ queryClient });
  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    const getArtifactText = spyOn(api, "getArtifactText");
    const readsBefore = getArtifactText.mock.calls.length;
    await act(async () => {
      await queryClient.invalidateQueries({ queryKey: ["artifact", artifact.id, "text"] });
    });
    expect(getArtifactText.mock.calls.length).toBeGreaterThan(readsBefore);
    expect(connections).toHaveLength(1);
    expect(connections[0]?.destroyed).toBe(false);
    expect(editors[0]?.destroyed).toBe(false);
  } finally {
    view.unmount();
  }
});

// A refetch of the text that fails - a server error, a store outage the server answers 503, or a
// network that drops - says nothing about the connection the editor already holds, which may be
// carrying edits made while its socket was down. The editor and its socket stay; only an
// admission's own read decides whether to connect.
test.each([
  ["a server error", new ApiError(500, { code: "INTERNAL", error: "internal server error" })],
  [
    "a store outage",
    new ApiError(503, {
      code: "DOC_SERVICE_UNAVAILABLE",
      error: "document service is unavailable",
    }),
  ],
  ["a network that drops", new TypeError("Failed to fetch")],
])("ProofDocument keeps its connected editor when a later text refetch meets %s", async (_name, failure) => {
  const queryClient = createQueryClient();
  const { connections, editors, sync, toolbar, view } = renderProofDocument({ queryClient });
  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    const getArtifactText = spyOn(api, "getArtifactText").mockRejectedValue(failure);
    const readsBefore = getArtifactText.mock.calls.length;
    await act(async () => {
      await queryClient.invalidateQueries({ queryKey: ["artifact", artifact.id, "text"] });
    });
    expect(getArtifactText.mock.calls.length).toBeGreaterThan(readsBefore);
    expect(queryClient.getQueryState(["artifact", artifact.id, "text"])?.status).toBe("error");
    expect(connections).toHaveLength(1);
    expect(connections[0]?.destroyed).toBe(false);
    expect(editors[0]?.destroyed).toBe(false);
    expect(within(view.container).queryByRole("alert")).not.toBeInTheDocument();
    expect(within(view.container).getByRole("article").hidden).toBe(false);
    expect(toolbar.current?.connection).not.toBe("failed");
  } finally {
    view.unmount();
  }
});

// The server refuses a socket into a room outside the Proof schema. The mounted editor closes, and
// a fresh read decides what follows: the repair it names, or a new connection once it reads.
test("ProofDocument closes a refused socket and shows the repair a fresh read names", async () => {
  const { connections, editors, refuse, sync, view } = renderProofDocument();
  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    spyOn(api, "getArtifactText").mockRejectedValue(
      new ApiError(409, { code: "DOC_SCHEMA", error: SCHEMA_REFUSAL })
    );
    act(() => refuse());

    expect(await screen.findByText(SCHEMA_REFUSAL)).not.toBeNull();
    expect(connections).toHaveLength(1);
    expect(connections[0]?.destroyed).toBe(true);
    expect(editors[0]?.destroyed).toBe(true);
  } finally {
    view.unmount();
  }
});

test("ProofDocument reconnects after a refused socket when the fresh read succeeds", async () => {
  const { connections, editors, refuse, sync, view } = renderProofDocument();
  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    act(() => refuse());

    await waitFor(() => expect(connections).toHaveLength(2));
    expect(connections[0]?.destroyed).toBe(true);
    expect(editors[0]?.destroyed).toBe(true);
    expect(connections[1]?.destroyed).toBe(false);
  } finally {
    view.unmount();
  }
});

/** The timers a refused socket schedules, held rather than run: `refuse` closes the socket the
 * server refused, and the page decides then, synchronously, when it reads the text again. */
function refuseHoldingTimers(refuse: () => void): { delays: number[]; release(): void } {
  const held: Array<() => void> = [];
  const delays: number[] = [];
  const setTimeoutSpy = spyOn(window, "setTimeout").mockImplementation(((
    handler: TimerHandler,
    delay?: number
  ) => {
    if (typeof handler === "function") {
      held.push(handler as () => void);
    }
    delays.push(delay ?? 0);
    return 0;
  }) as typeof window.setTimeout);
  try {
    act(() => refuse());
  } finally {
    setTimeoutSpy.mockRestore();
  }
  return {
    delays,
    release() {
      for (const handler of held) {
        handler();
      }
    },
  };
}

// A socket the server keeps refusing while the text reads is retried after 0, 1, 2, 4 and 8
// seconds, and the sixth refusal in a row stops reconnecting and says so.
test("ProofDocument backs off from a socket the server keeps refusing and stops at the sixth", async () => {
  const { connections, refuse, sync, toolbar, view } = renderProofDocument();
  try {
    await sync();
    for (const [index, delay] of [0, 1_000, 2_000, 4_000, 8_000].entries()) {
      const refusal = refuseHoldingTimers(refuse);
      expect(refusal.delays).toEqual([delay]);
      expect(connections).toHaveLength(index + 1);
      act(() => refusal.release());
      await waitFor(() => expect(connections).toHaveLength(index + 2));
    }
    expect(refuseHoldingTimers(refuse).delays).toEqual([]);
    expect(
      await within(view.container).findByText(
        "This document could not load: the server keeps refusing its connection; reload the page to try again"
      )
    ).not.toBeNull();
    expect(connections).toHaveLength(6);
    expect(connections.every((connection) => connection.destroyed)).toBe(true);
    await waitFor(() => expect(toolbar.current?.connection).toBe("failed"));
  } finally {
    view.unmount();
  }
});

// A socket that syncs ends the run of refusals, so a later refusal starts the backoff again rather
// than counting toward the stop.
test("ProofDocument starts the refusal backoff again once a socket syncs", async () => {
  const fake = fakeDocumentRuntime({ text: "The live document" });
  const { connections, refuse, sync, view } = renderProofDocument({ fake });
  try {
    await sync();
    for (const [index, delay] of [0, 1_000].entries()) {
      const refusal = refuseHoldingTimers(refuse);
      expect(refusal.delays).toEqual([delay]);
      act(() => refusal.release());
      await waitFor(() => expect(connections).toHaveLength(index + 2));
    }
    act(() => fake.sync());
    expect(refuseHoldingTimers(refuse).delays).toEqual([0]);
  } finally {
    view.unmount();
  }
});

/** What Dispatch answers a rebuild that restored the document's latest saved version. */
const REBUILT_REPORT: ArtifactRebuildReport = {
  head: 1,
  removed_checkpoints: 0,
  removed_snapshots: 0,
  removed_updates: 3,
  source_version: 1,
  validation_error: "decode live document: unexpected end of input",
};

// Only a stored history that cannot load is rebuilt: the page offers the rebuild for the code the
// server answers that state with, and the read after the rebuild admits the editor again.
test("ProofDocument offers a rebuild for a history that cannot load and reconnects after it", async () => {
  const user = userEvent.setup();
  const getArtifactText = spyOn(api, "getArtifactText").mockRejectedValue(
    new ApiError(409, { code: "DOCUMENT_UNLOADABLE", error: UNLOADABLE_REFUSAL })
  );
  const rebuildArtifact = spyOn(api, "rebuildArtifact").mockResolvedValue(REBUILT_REPORT);
  const confirm = spyOn(window, "confirm").mockReturnValue(true);
  const { connections, view } = renderProofDocument();
  try {
    expect(await screen.findByText(UNLOADABLE_REFUSAL)).not.toBeNull();
    expect(connections).toHaveLength(0);
    getArtifactText.mockResolvedValue({
      markdown: "The live document",
      token: "sha256:live-document",
      version: 1,
    });
    await user.click(screen.getByRole("button", { name: "Rebuild from the latest version" }));
    await waitFor(() => expect(rebuildArtifact).toHaveBeenCalledTimes(1));
    expect(rebuildArtifact.mock.calls[0]).toEqual([artifact.id, undefined, keyedAttempt]);
    await waitFor(() => expect(connections).toHaveLength(1));
    expect(
      screen.queryByRole("button", { name: "Rebuild from the latest version" })
    ).not.toBeInTheDocument();
  } finally {
    view.unmount();
    confirm.mockRestore();
    rebuildArtifact.mockRestore();
    getArtifactText.mockRestore();
  }
});

// A rebuild the reader stopped may have been made: the page says so, and that its button is a
// press of its own, rather than the stopped write's own words, which are a composer's.
test("a rebuild the reader stopped says it may have been made, not a message's words", async () => {
  const getArtifactText = spyOn(api, "getArtifactText").mockRejectedValue(
    new ApiError(409, {
      code: "DOCUMENT_UNLOADABLE",
      error: "the document's stored history cannot load; rebuild it from its latest saved version",
    })
  );
  const rebuildArtifact = spyOn(api, "rebuildArtifact").mockRejectedValue(
    new NetworkError("/api/v1/artifacts/rebuild", new TypeError("Failed to fetch"))
  );
  const confirm = spyOn(window, "confirm").mockReturnValue(true);
  const queryClient = createQueryClient();
  const { view } = renderProofDocument({ queryClient });
  try {
    await screen.findByRole("button", { name: "Rebuild from the latest version" });
    jest.useFakeTimers();
    await userOnFakeClock().click(
      screen.getByRole("button", { name: "Rebuild from the latest version" })
    );
    await stopTheLateWrite(queryClient);
    expect(
      screen.getByText(
        stoppedWriteNote(`the rebuild of ${artifact.name}`, "made", {
          anew: "Rebuilding again sends a new rebuild.",
        })
      )
    ).not.toBeNull();
  } finally {
    realClock();
    view.unmount();
    queryClient.clear();
    confirm.mockRestore();
    rebuildArtifact.mockRestore();
    getArtifactText.mockRestore();
  }
});

test("Rebuild lets go at the document write deadline and sends again", async () => {
  const getArtifactText = spyOn(api, "getArtifactText").mockRejectedValue(
    new ApiError(409, { code: "DOCUMENT_UNLOADABLE", error: UNLOADABLE_REFUSAL })
  );
  const rebuild = spyOn(api, "rebuildArtifact").mockRejectedValue(
    new NetworkError("/api/v1/artifacts/rebuild", new TypeError("Failed to fetch"))
  );
  const confirm = spyOn(window, "confirm").mockReturnValue(true);
  const queryClient = createQueryClient();
  const { view } = renderProofDocument({ queryClient });
  try {
    const name = "Rebuild from the latest version";
    const button = await screen.findByRole<HTMLButtonElement>("button", { name });
    jest.useFakeTimers();
    const user = userOnFakeClock();
    const sent = () => pressesOf([rebuild]);
    await heldUntilLate(user, button, {
      label: `Rebuild of ${artifact.name}`,
      name,
      queryClient,
      sent,
    });
    await user.keyboard("{Enter}");
    await tick(1_000);
    expect(sent()).toBe(2);
  } finally {
    realClock();
    view.unmount();
    queryClient.clear();
    confirm.mockRestore();
    rebuild.mockRestore();
    getArtifactText.mockRestore();
  }
});

/** The app's own query client, with reads fresh until a write invalidates them, as the other rows
 *  of this file give the page theirs (`createQueryClient`). */
function freshReadsQueryClient(): DispatchQueryClient {
  return new DispatchQueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
}

// A rebuild that lands as the browser goes offline has the reads it made stale paused by the
// network, with no deadline of their own running: the app's client holds a read of an idle query
// until its paused fetch answers (`DispatchQueryClient`). Inside `NoTooltips` Rebuild is held
// through them, so it holds them only up to the request deadline (`REQUEST_DEADLINE_MS`), and
// lets go there with the focus on it. Once the network returns the text reads, and the offer goes.
test("offline, inside NoTooltips, Rebuild is held through the reads its landed rebuild made only until the request deadline", async () => {
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue(artifact);
  const getArtifactBlocks = spyOn(api, "getArtifactBlocks").mockResolvedValue([]);
  const getArtifactText = spyOn(api, "getArtifactText").mockRejectedValue(
    new ApiError(409, { code: "DOCUMENT_UNLOADABLE", error: UNLOADABLE_REFUSAL })
  );
  const rebuilt = Promise.withResolvers<ArtifactRebuildReport>();
  const rebuildArtifact = spyOn(api, "rebuildArtifact").mockImplementation(() => rebuilt.promise);
  const confirm = spyOn(window, "confirm").mockReturnValue(true);
  const queryClient = freshReadsQueryClient();
  const { view } = renderProofDocument({ noTooltips: true, queryClient });
  const online = onlineManager.isOnline();
  const stopFocusFixup = emulateChromiumFocusFixup();
  try {
    const name = "Rebuild from the latest version";
    const button = await screen.findByRole<HTMLButtonElement>("button", { name });
    jest.useFakeTimers();
    await pressHeld(userOnFakeClock(), button, {
      name,
      sent: () => rebuildArtifact.mock.calls.length,
    });

    getArtifactText.mockResolvedValue({
      markdown: "The live document",
      token: "sha256:live-document",
      version: 1,
    });
    onlineManager.setOnline(false);
    await act(async () => rebuilt.resolve(REBUILT_REPORT));
    await tick(0);
    expect(queryClient.getQueryState(["artifact", artifact.id, "text"])?.fetchStatus).toBe(
      "paused"
    );

    // The rebuild landed in the step before, which is when the hold's timer started: two seconds
    // short of the request deadline the reads are still paused, and Rebuild held.
    await tick(REQUEST_DEADLINE_MS - 2_000);
    expect(heldNotDisabled(button, name)).toBe(true);
    expect(button).toHaveFocus();

    // At it Rebuild lets go, with the focus on it, and the rebuild is not sent again.
    await tick(2_000);
    expect(heldNotDisabled(button, name)).toBe(false);
    expect(button).toHaveFocus();
    expect(rebuildArtifact).toHaveBeenCalledTimes(1);

    // The network returns, the text reads, and the offer leaves the page.
    act(() => onlineManager.setOnline(true));
    await waitFor(() => expect(screen.queryByRole("button", { name })).not.toBeInTheDocument());
  } finally {
    // The page and its reads go before the network returns, so no read paused offline goes on.
    view.unmount();
    queryClient.clear();
    onlineManager.setOnline(online);
    stopFocusFixup();
    realClock();
    confirm.mockRestore();
    rebuildArtifact.mockRestore();
    getArtifactText.mockRestore();
    getArtifactBlocks.mockRestore();
    getArtifact.mockRestore();
  }
});

// The same hold where the network answers: Rebuild stays held, with the focus, through the read
// of the rebuilt document's text for as long as that read takes, so a press then sends no second
// rebuild; once it answers, the offer leaves the page and the editor connects.
test("inside NoTooltips, Rebuild stays held through the read after its rebuild until that read answers", async () => {
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue(artifact);
  const getArtifactBlocks = spyOn(api, "getArtifactBlocks").mockResolvedValue([]);
  const getArtifactText = spyOn(api, "getArtifactText").mockRejectedValue(
    new ApiError(409, { code: "DOCUMENT_UNLOADABLE", error: UNLOADABLE_REFUSAL })
  );
  const rebuilt = Promise.withResolvers<ArtifactRebuildReport>();
  const rebuildArtifact = spyOn(api, "rebuildArtifact").mockImplementation(() => rebuilt.promise);
  const confirm = spyOn(window, "confirm").mockReturnValue(true);
  const reread = Promise.withResolvers<ArtifactText>();
  const queryClient = freshReadsQueryClient();
  const { connections, view } = renderProofDocument({ noTooltips: true, queryClient });
  const stopFocusFixup = emulateChromiumFocusFixup();
  try {
    const name = "Rebuild from the latest version";
    const button = await screen.findByRole<HTMLButtonElement>("button", { name });
    await pressHeld(userEvent.setup(), button, {
      name,
      sent: () => rebuildArtifact.mock.calls.length,
    });

    const readsBefore = getArtifactText.mock.calls.length;
    getArtifactText.mockImplementation(() => reread.promise);
    await act(async () => rebuilt.resolve(REBUILT_REPORT));
    await waitFor(() => expect(getArtifactText.mock.calls.length).toBeGreaterThan(readsBefore));
    expect(heldNotDisabled(button, name)).toBe(true);
    expect(button).toHaveFocus();
    expect(rebuildArtifact).toHaveBeenCalledTimes(1);

    await act(async () =>
      reread.resolve({ markdown: "The live document", token: "sha256:live-document", version: 1 })
    );
    await waitFor(() => expect(connections).toHaveLength(1));
    expect(screen.queryByRole("button", { name })).not.toBeInTheDocument();
    expect(rebuildArtifact).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    queryClient.clear();
    stopFocusFixup();
    confirm.mockRestore();
    rebuildArtifact.mockRestore();
    getArtifactText.mockRestore();
    getArtifactBlocks.mockRestore();
    getArtifact.mockRestore();
  }
});

// Inside `NoTooltips` a read of the rebuilt document that has not answered by the request deadline
// lets Rebuild go there, with the focus on it, and the offer says why beside Rebuild, in the words
// of the error such a read ends with. The line goes once that read settles: here it answers with
// the same refusal, so the offer stays and Rebuild with it.
test("inside NoTooltips, a rebuild whose read has not answered by the request deadline lets Rebuild go there, and the offer says why until that read settles", async () => {
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue(artifact);
  const getArtifactBlocks = spyOn(api, "getArtifactBlocks").mockResolvedValue([]);
  const getArtifactText = spyOn(api, "getArtifactText").mockRejectedValue(
    new ApiError(409, { code: "DOCUMENT_UNLOADABLE", error: UNLOADABLE_REFUSAL })
  );
  const rebuilt = Promise.withResolvers<ArtifactRebuildReport>();
  const rebuildArtifact = spyOn(api, "rebuildArtifact").mockImplementation(() => rebuilt.promise);
  const confirm = spyOn(window, "confirm").mockReturnValue(true);
  const reread = Promise.withResolvers<ArtifactText>();
  const queryClient = freshReadsQueryClient();
  const { view } = renderProofDocument({ noTooltips: true, queryClient });
  const stopFocusFixup = emulateChromiumFocusFixup();
  const why = "The rebuilt document is not read yet. Dispatch did not answer within 30 seconds.";
  try {
    const name = "Rebuild from the latest version";
    const button = await screen.findByRole<HTMLButtonElement>("button", { name });
    jest.useFakeTimers();
    await pressHeld(userOnFakeClock(), button, {
      name,
      sent: () => rebuildArtifact.mock.calls.length,
    });

    getArtifactText.mockImplementation(() => reread.promise);
    await act(async () => rebuilt.resolve(REBUILT_REPORT));
    await tick(0);

    // The rebuild landed in the step before, which is when the hold's timer started: two seconds
    // short of the request deadline the read is still out, Rebuild held, and nothing said.
    await tick(REQUEST_DEADLINE_MS - 2_000);
    expect(heldNotDisabled(button, name)).toBe(true);
    expect(screen.queryByText(why)).toBeNull();

    // At it Rebuild lets go, with the focus on it, and the offer says why beside it.
    await tick(2_000);
    expect(heldNotDisabled(button, name)).toBe(false);
    expect(button).toHaveFocus();
    expect(screen.getByText(why).parentElement).toBe(button.parentElement);

    // The read settles, refused again: the line goes, and the offer and its Rebuild stay.
    await act(async () =>
      reread.reject(new ApiError(409, { code: "DOCUMENT_UNLOADABLE", error: UNLOADABLE_REFUSAL }))
    );
    await tick(0);
    expect(screen.queryByText(why)).toBeNull();
    expect(screen.getByRole("button", { name })).toBe(button);
    expect(rebuildArtifact).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    queryClient.clear();
    stopFocusFixup();
    realClock();
    confirm.mockRestore();
    rebuildArtifact.mockRestore();
    getArtifactText.mockRestore();
    getArtifactBlocks.mockRestore();
    getArtifact.mockRestore();
  }
});

// A row's editor is drawn only while a row is edited in it, so the editor closed and opened again
// is a new one while a rebuild's read of the document is still out. Its own admission waits on
// that read, so it draws no offer and no Rebuild to press a second time until the read settles.
// The read has one hold for every editor of the document (`useReadBound`): at the bound the
// editor drawn again says why the read has not settled, as the editor it was pressed in would
// have, until the read settles and the admission draws the offer again, with Rebuild free and the
// rebuild sent once (the tester's row of `issue-page-i.e2e.ts`).
test("inside NoTooltips, an editor drawn again while its rebuild's read is out says why at the bound until the read settles", async () => {
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue(artifact);
  const getArtifactBlocks = spyOn(api, "getArtifactBlocks").mockResolvedValue([]);
  const refusal = () =>
    new ApiError(409, { code: "DOCUMENT_UNLOADABLE", error: UNLOADABLE_REFUSAL });
  const getArtifactText = spyOn(api, "getArtifactText").mockRejectedValue(refusal());
  const rebuilt = Promise.withResolvers<ArtifactRebuildReport>();
  const rebuildArtifact = spyOn(api, "rebuildArtifact").mockImplementation(() => rebuilt.promise);
  const confirm = spyOn(window, "confirm").mockReturnValue(true);
  const reread = Promise.withResolvers<ArtifactText>();
  const queryClient = freshReadsQueryClient();
  // The editor as it is drawn now: the one the press is made in, then the one drawn again.
  let drawn = renderProofDocument({ noTooltips: true, queryClient }).view;
  const why = "The rebuilt document is not read yet. Dispatch did not answer within 30 seconds.";
  const rebuildOf = () =>
    screen.getByRole<HTMLButtonElement>("button", {
      name: /^(Rebuild from the latest version|Rebuilding…)$/,
    });
  try {
    await screen.findByRole("button", { name: "Rebuild from the latest version" });
    jest.useFakeTimers();
    await userOnFakeClock().click(rebuildOf());
    await tick(0);
    expect(rebuildArtifact).toHaveBeenCalledTimes(1);
    getArtifactText.mockImplementation(() => reread.promise);
    await act(async () => rebuilt.resolve(REBUILT_REPORT));
    await tick(0);

    drawn.unmount();
    drawn = renderProofDocument({ noTooltips: true, queryClient }).view;
    await tick(1_000);
    expect(
      screen.queryByRole("button", { name: /^Rebuild/ }),
      "the editor drawn again offered Rebuild while the rebuild's read was out"
    ).toBeNull();
    expect(screen.queryByText(why)).toBeNull();

    await tick(REQUEST_DEADLINE_MS);
    expect(
      screen.queryByText(why),
      "the editor drawn again said nothing of the rebuild's read at the bound"
    ).toBeInTheDocument();

    await act(async () => reread.reject(refusal()));
    await tick(0);
    expect(screen.queryByText(why)).toBeNull();
    expect(heldNotDisabled(rebuildOf(), "Rebuild")).toBe(false);
    expect(rebuildArtifact).toHaveBeenCalledTimes(1);
  } finally {
    drawn.unmount();
    queryClient.clear();
    realClock();
    confirm.mockRestore();
    rebuildArtifact.mockRestore();
    getArtifactText.mockRestore();
    getArtifactBlocks.mockRestore();
    getArtifact.mockRestore();
  }
});

// The row's editor closed and opened again while a rebuild's write is still out, before it lands,
// draws the offer again: the rebuild has not landed, so the document's text is refused as before.
// Its Rebuild holds from that rebuild's own state (`usePendingKeyedPress`), as the Rebuild pressed
// did: `Rebuilding…`, a press of it asks nothing and sends no second rebuild, and it lets go at
// the rebuild's deadline (`SEND_DEADLINE_MS`). Dispatch never answers the rebuild: each try fails
// at the network and is tried again under its key.
test("inside NoTooltips, an editor drawn again while its rebuild's write is out holds Rebuild until the rebuild's deadline and sends no second rebuild", async () => {
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue(artifact);
  const getArtifactBlocks = spyOn(api, "getArtifactBlocks").mockResolvedValue([]);
  const getArtifactText = spyOn(api, "getArtifactText").mockRejectedValue(
    new ApiError(409, { code: "DOCUMENT_UNLOADABLE", error: UNLOADABLE_REFUSAL })
  );
  const rebuild = spyOn(api, "rebuildArtifact").mockRejectedValue(
    new NetworkError("/api/v1/artifacts/rebuild", new TypeError("Failed to fetch"))
  );
  const confirm = spyOn(window, "confirm").mockReturnValue(true);
  const queryClient = freshReadsQueryClient();
  // The editor as it is drawn now: the one the press is made in, then the one drawn again.
  let drawn = renderProofDocument({ noTooltips: true, queryClient }).view;
  const rebuildOf = () =>
    screen.getByRole<HTMLButtonElement>("button", {
      name: /^(Rebuild from the latest version|Rebuilding…)$/,
    });
  const sent = () => pressesOf([rebuild]);
  try {
    await screen.findByRole("button", { name: "Rebuild from the latest version" });
    jest.useFakeTimers();
    const user = userOnFakeClock();
    await user.click(rebuildOf());
    await tick(0);
    expect(sent()).toBe(1);

    drawn.unmount();
    drawn = renderProofDocument({ noTooltips: true, queryClient }).view;
    await tick(1_000);
    expect(
      rebuildOf().textContent,
      "the editor drawn again let Rebuild go while the rebuild's write was out"
    ).toBe("Rebuilding…");
    expect(heldNotDisabled(rebuildOf(), "Rebuild")).toBe(true);
    await user.click(rebuildOf());
    await tick(0);
    expect(sent(), "a press of the Rebuild drawn again sent a second rebuild").toBe(1);
    expect(confirm).toHaveBeenCalledTimes(1);

    // A second short of the rebuild's deadline, counted from its press.
    await tick(SEND_DEADLINE_MS - 2_000);
    expect(heldNotDisabled(rebuildOf(), "Rebuild")).toBe(true);
    await tick(2_000);
    expect(heldNotDisabled(rebuildOf(), "Rebuild")).toBe(false);
  } finally {
    drawn.unmount();
    queryClient.clear();
    realClock();
    confirm.mockRestore();
    rebuild.mockRestore();
    getArtifactText.mockRestore();
    getArtifactBlocks.mockRestore();
    getArtifact.mockRestore();
  }
});

// The line a rebuild's read the bound ended draws is that rebuild's own: the next rebuild takes it
// away as it lands, though the earlier read is still out, and that rebuild's read answering says
// nothing more. Dispatch holds the first rebuild's read of the text until the row answers it, so
// the next rebuild's read, which waits for that one (`DispatchQueryClient`), answers after it:
// here refused again, so the offer and its Rebuild stay.
test("inside NoTooltips, the line a rebuild's read the bound ended draws goes as the next rebuild lands, whose read answers", async () => {
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue(artifact);
  const getArtifactBlocks = spyOn(api, "getArtifactBlocks").mockResolvedValue([]);
  const refusal = () =>
    new ApiError(409, { code: "DOCUMENT_UNLOADABLE", error: UNLOADABLE_REFUSAL });
  const getArtifactText = spyOn(api, "getArtifactText").mockRejectedValue(refusal());
  // Each rebuild's answer, held until the row gives it, so what it sets off runs in `act`.
  const { answers: rebuilds, next } = heldAnswers<ArtifactRebuildReport>();
  const rebuildArtifact = spyOn(api, "rebuildArtifact").mockImplementation(next);
  const confirm = spyOn(window, "confirm").mockReturnValue(true);
  const reread = Promise.withResolvers<ArtifactText>();
  const queryClient = freshReadsQueryClient();
  const { view } = renderProofDocument({ noTooltips: true, queryClient });
  const why = "The rebuilt document is not read yet. Dispatch did not answer within 30 seconds.";
  try {
    const name = "Rebuild from the latest version";
    const button = await screen.findByRole<HTMLButtonElement>("button", { name });
    jest.useFakeTimers();
    const user = userOnFakeClock();
    await user.click(button);
    await tick(0);
    getArtifactText.mockImplementationOnce(() => reread.promise);
    await act(async () => rebuilds[0]?.(REBUILT_REPORT));
    await tick(0);
    await tick(REQUEST_DEADLINE_MS);
    expect(heldNotDisabled(button, name)).toBe(false);
    expect(screen.getByText(why).parentElement).toBe(button.parentElement);

    await user.click(button);
    await tick(0);
    expect(rebuildArtifact).toHaveBeenCalledTimes(2);
    await act(async () => rebuilds[1]?.(REBUILT_REPORT));
    await tick(0);
    expect(heldNotDisabled(button, name)).toBe(true);
    expect(
      screen.queryByText(why),
      "the earlier rebuild's read kept its line past the next rebuild"
    ).toBeNull();

    await act(async () => reread.reject(refusal()));
    await tick(0);
    expect(heldNotDisabled(button, name)).toBe(false);
    expect(screen.queryByText(why)).toBeNull();
    expect(screen.getByRole("button", { name })).toBe(button);
  } finally {
    view.unmount();
    queryClient.clear();
    realClock();
    confirm.mockRestore();
    rebuildArtifact.mockRestore();
    getArtifactText.mockRestore();
    getArtifactBlocks.mockRestore();
    getArtifact.mockRestore();
  }
});

// A room or store that could not serve the document is not a history to discard: the page says
// the document could not load, and offers no rebuild.
test("ProofDocument offers no rebuild for a document service that is unavailable", async () => {
  const getArtifactText = spyOn(api, "getArtifactText").mockRejectedValue(
    new ApiError(503, {
      code: "DOC_SERVICE_UNAVAILABLE",
      error: "document service is unavailable",
    })
  );
  const { connections, view } = renderProofDocument();
  try {
    expect(
      await within(view.container).findByText(
        "This document could not load: document service is unavailable"
      )
    ).not.toBeNull();
    expect(connections).toHaveLength(0);
    expect(
      screen.queryByRole("button", { name: "Rebuild from the latest version" })
    ).not.toBeInTheDocument();
  } finally {
    view.unmount();
    getArtifactText.mockRestore();
  }
});

// A document holding content this build of Dispatch cannot read was written by a newer build
// during a release. Nothing on this page repairs it, and a replacement would delete that content,
// so the page offers no upload or rebuild: it says the document reloads, reads the text again on
// the refusal schedule, and connects once a read succeeds.
test("ProofDocument shows content this build cannot show yet without a replace drop zone, and reloads", async () => {
  const getArtifactText = spyOn(api, "getArtifactText").mockRejectedValue(
    new ApiError(409, {
      code: "DOC_NEWER_CONTENT",
      error:
        "this Dispatch process's build cannot read an element of this document. Try again in one minute.",
    })
  );
  const { connections, view } = renderProofDocument();
  try {
    expect(
      await within(view.container).findByText(
        "This document holds content this version of Dispatch cannot show yet. It reloads in a minute."
      )
    ).not.toBeNull();
    expect(connections).toHaveLength(0);
    expect(screen.queryByLabelText("Upload artifact")).toBeNull();
    expect(screen.queryByText("Upload markdown to replace and repair this document.")).toBeNull();
    expect(screen.queryByRole("button", { name: "Rebuild from the latest version" })).toBeNull();
    expect(within(view.container).queryByText(/This document could not load/)).toBeNull();
    await waitFor(() => expect(getArtifactText.mock.calls.length).toBeGreaterThan(1));

    getArtifactText.mockResolvedValue({
      markdown: "The live document",
      token: "sha256:live-document",
      version: 1,
    });
    await waitFor(() => expect(connections).toHaveLength(1), { timeout: 3_000 });
    expect(
      within(view.container).queryByText(
        "This document holds content this version of Dispatch cannot show yet. It reloads in a minute."
      )
    ).toBeNull();
  } finally {
    view.unmount();
    getArtifactText.mockRestore();
  }
});

// A reading column (the Inbox's) is not the document's page: it offers no repair and no rebuild,
// because a reader beside a list of questions has not come to mend a document. Each blocked
// admission is reported to the host instead, which shows the question's own card in its place.
test("a pane draws neither prompt for a blocked admission and reports it to its host", async () => {
  const repair = new ApiError(409, { code: "DOC_SCHEMA", error: SCHEMA_REFUSAL });
  const getArtifactText = spyOn(api, "getArtifactText").mockRejectedValue(repair);
  const reported: string[] = [];
  const schema = renderProofDocument({
    onUnavailable: (reason) => reported.push(reason),
    surface: "pane",
  });
  try {
    await waitFor(() => expect(reported).toEqual([SCHEMA_REFUSAL]));
    expect(
      screen.queryByText("Upload markdown to replace and repair this document.")
    ).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Upload artifact")).not.toBeInTheDocument();
    expect(schema.connections).toHaveLength(0);
  } finally {
    schema.view.unmount();
  }

  getArtifactText.mockRejectedValue(
    new ApiError(409, { code: "DOCUMENT_UNLOADABLE", error: UNLOADABLE_REFUSAL })
  );
  const history = renderProofDocument({
    onUnavailable: (reason) => reported.push(reason),
    surface: "pane",
  });
  try {
    await waitFor(() => expect(reported).toHaveLength(2));
    expect(reported[1]).toBe(UNLOADABLE_REFUSAL);
    expect(
      screen.queryByRole("button", { name: "Rebuild from the latest version" })
    ).not.toBeInTheDocument();
    expect(history.connections).toHaveLength(0);
  } finally {
    history.view.unmount();
    getArtifactText.mockRestore();
  }
});

test("a transport that fails to load is reported instead of connecting forever", async () => {
  const fake = fakeDocumentRuntime({ text: "The live document" });
  const rejection = Promise.reject(new TypeError("Failed to fetch"));
  rejection.catch(() => undefined); // avoid an unhandled-rejection warning before the fake below is read
  fake.runtime.loadTransport = () => rejection;
  const { toolbar, view } = renderProofDocument({ fake });

  try {
    await flushLoadFailure(rejection);
    const alert = within(view.container).getByRole("alert");
    expect(alert.textContent).toBe("This document could not load: Failed to fetch");
    expect(toolbar.current?.connection).toBe("failed");
  } finally {
    view.unmount();
  }
});

test("an editor that fails to load after sync is reported the same way, and later provider status stays out of the way", async () => {
  const fake = fakeDocumentRuntime({ text: "The live document" });
  const rejection = Promise.reject(new Error("editor chunk missing"));
  rejection.catch(() => undefined); // avoid an unhandled-rejection warning before the fake below is read
  fake.runtime.createEditor = () => rejection;
  const { status, sync, toolbar, view } = renderProofDocument({ fake });

  try {
    await sync();
    await flushLoadFailure(rejection);
    const alert = within(view.container).getByRole("alert");
    expect(alert.textContent).toBe("This document could not load: editor chunk missing");
    expect(toolbar.current?.connection).toBe("failed");
    // The live connection is still up and may reconnect; its dot must not contradict the alert.
    act(() => status("connected"));
    expect(toolbar.current?.connection).toBe("failed");
    expect(within(view.container).getByRole("alert")).toHaveTextContent("editor chunk missing");
  } finally {
    view.unmount();
  }
});

test("ProofDocument makes a schema-read-only admission non-editable and reloadable", async () => {
  const { admit, connections, editors, status, sync, view } = renderProofDocument();

  try {
    await waitFor(() => expect(connections).toHaveLength(1));
    act(() => status("connected"));
    act(() => admit(true));
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    expect(editors[0]?.access).toBe("read");
    expect(within(view.container).getByRole("article")).toHaveAttribute("data-read-only", "true");
    expect(within(view.container).getByText("Reload to edit.")).toBeDefined();
    // A socket that is not connected names no reload: its reconnect's admission decides.
    act(() => status("offline"));
    expect(within(view.container).queryByText("Reload to edit.")).not.toBeInTheDocument();
    act(() => status("connected"));
    expect(within(view.container).getByText("Reload to edit.")).toBeDefined();
  } finally {
    view.unmount();
  }
});

// The server admits a closed issue's socket read-only as it admits a schema version it does not
// serve, and the page cannot tell those apart. While the issue is closed the page shows the
// closed line and no reload, which could not make that document editable. Once the issue is open
// again, a reload does make it editable, because the reload's new socket is admitted on the issue
// as it stands. The older rule - that a socket admitted while the issue was closed names no
// reload, not even after the reopen - was wrong for exactly that case: it left a reader of a
// reopened issue in front of a read-only editor with no line telling them what to do.
test("ProofDocument names a reload once a read-only admission's issue is open again", async () => {
  const { admit, connections, editors, rerender, status, sync, view } = renderProofDocument({
    isClosed: true,
  });
  const reloadLine = () => within(view.container).queryByText("Reload to edit.");
  const closedLine = () =>
    within(view.container).queryByText("This issue is closed. Its document is read-only.");

  try {
    await waitFor(() => expect(connections).toHaveLength(1));
    act(() => status("connected"));
    act(() => admit(true));
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    expect(editors[0]?.access).toBe("comment");
    expect(within(view.container).getByRole("article")).toHaveAttribute("data-read-only", "true");
    expect(closedLine()).toBeInTheDocument();
    expect(reloadLine()).not.toBeInTheDocument();

    // Reopened while that socket still holds the document: the editor stays read-only, and the
    // page shows the reload that makes it editable again.
    act(() => rerender({ isClosed: false }));
    expect(editors[0]?.access).toBe("read");
    expect(within(view.container).getByRole("article")).toHaveAttribute("data-read-only", "true");
    expect(closedLine()).not.toBeInTheDocument();
    expect(reloadLine()).toBeInTheDocument();

    // The drop the reopen may make, and the reconnect's draw: a socket that is not connected names
    // no reload, and the reconnect names it again until its own admission says otherwise.
    for (const step of ["offline", "connecting"] as const) {
      act(() => status(step));
      expect(reloadLine()).not.toBeInTheDocument();
    }
    act(() => status("connected"));
    expect(reloadLine()).not.toBeNull();

    // A writable admission names neither line.
    act(() => admit(false));
    expect(editors[0]?.access).toBe("edit");
    expect(within(view.container).getByRole("article")).toHaveAttribute("data-read-only", "false");
    expect(closedLine()).not.toBeInTheDocument();
    expect(reloadLine()).not.toBeInTheDocument();

    act(() => rerender({ isClosed: true }));
    expect(closedLine()).toBeInTheDocument();
    expect(reloadLine()).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

// The page cannot tell a close that dropped its socket from one that did not - a close served by
// another server process, or a room read that failed - so a close and a reopen on one socket leave
// its admission as it was: a schema's read-only admission still names the reload that fixes it.
test("ProofDocument names a reload again for a schema's admission when a close and a reopen leave its socket", async () => {
  const { admit, connections, editors, rerender, status, sync, view } = renderProofDocument();
  const reloadLine = () => within(view.container).queryByText("Reload to edit.");

  try {
    await waitFor(() => expect(connections).toHaveLength(1));
    act(() => status("connected"));
    act(() => admit(true));
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    expect(reloadLine()).not.toBeNull();

    act(() => rerender({ isClosed: true }));
    expect(reloadLine()).not.toBeInTheDocument();
    act(() => rerender({ isClosed: false }));
    expect(editors[0]?.access).toBe("read");
    expect(reloadLine()).not.toBeNull();
  } finally {
    view.unmount();
  }
});

// During a release the process that answers the page can be of the other build, and its schema
// route names that build's manifest, not the one this page's editor builds. So the page presents
// the manifest it was built with: its version, and the highest version that reads every element
// it creates (`compiledSchemaCreatable`), and it asks no server for a schema.
test("ProofDocument connects with the schema it was built with and reads none from the server", async () => {
  const realFetch = globalThis.fetch;
  const schemaReads: string[] = [];
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const path = input.toString();
    if (path.startsWith("/api/v1/schema/blocks")) {
      schemaReads.push(path);
      return Response.json({ ...manifest, version: manifest.version + 1 });
    }
    return realFetch(input, init);
  }) as unknown as typeof fetch;
  const { callbacks, connections, view } = renderProofDocument();

  try {
    await waitFor(() => expect(connections).toHaveLength(1));
    expect(callbacks[0]?.schemaVersion).toBe(manifest.version);
    expect(callbacks[0]?.schemaCreatable).toBe(compiledSchemaCreatable);
    expect(schemaReads).toEqual([]);
  } finally {
    view.unmount();
    globalThis.fetch = realFetch;
  }
});

// The server admits every socket on a closed issue's document read-only, so that admission says
// nothing about this editor's schema, and the document takes comments by quote through it. On an
// open issue a read-only admission means an outdated schema, and the document takes nothing.
test("a closed issue's document takes comments by quote through its read-only admission; an open one's does not", async () => {
  const { admit, connections, editors, rerender, sync, view } = renderProofDocument({
    isClosed: true,
  });

  try {
    await waitFor(() => expect(connections).toHaveLength(1));
    act(() => admit(true));
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    expect(editors[0]?.options).toMatchObject({ access: "comment" });
    act(() => admit(true));
    expect(editors[0]?.access).toBe("comment");

    rerender({ isClosed: false });
    expect(editors[0]?.access).toBe("read");
  } finally {
    view.unmount();
  }
});

// The server decides a socket's admission once, as it connects, and admits every socket on a
// closed issue read-only. Reopened from the page, the issue's document is admitted again: it stays
// read-only until that admission arrives, and takes edits once it is read-write. A close asks
// nothing of the page, since the server closes the issue's rooms itself.
test("a reopened issue's document is admitted again and takes edits once that admission is read-write", async () => {
  const { admit, connections, editors, rerender, sync, view } = renderProofDocument({
    isClosed: true,
  });

  try {
    await waitFor(() => expect(connections).toHaveLength(1));
    act(() => admit(true));
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    expect(connections[0]?.readmitted).toBe(0);

    rerender({ isClosed: false });
    expect(connections[0]?.readmitted).toBe(1);
    expect(editors[0]?.access).toBe("read");

    act(() => admit(false));
    expect(editors[0]?.access).toBe("edit");
    expect(within(view.container).getByRole("article").getAttribute("data-read-only")).toBe(
      "false"
    );
    expect(within(view.container).queryByText("Reload to edit.")).toBeNull();

    rerender({ isClosed: false });
    rerender({ isClosed: true });
    expect(connections[0]?.readmitted).toBe(1);
    expect(connections).toHaveLength(1);
    expect(editors[0]?.destroyed).toBe(false);
  } finally {
    view.unmount();
  }
});
