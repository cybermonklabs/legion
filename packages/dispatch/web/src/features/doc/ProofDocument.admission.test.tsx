import { beforeEach, expect, spyOn, test } from "bun:test";
import type { QueryClient } from "@tanstack/react-query";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { fakeDocumentRuntime } from "../../__tests__/document-runtime";
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
import { ApiError, api } from "../../api/client";
import type { ArtifactText } from "../../api/types";
import { plainWrite, writesOf } from "../writes/write-store";
import { openPendingEdits } from "./pending-edits";

// Whether the live editor may connect, and what the page shows when it may not or cannot: the
// admission's own `/text` read, a socket the server refuses as outside the Proof schema, the repair
// and rebuild it offers, and a transport or editor that fails to load.

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
    new ApiError(409, {
      code: "DOC_SCHEMA",
      error:
        "document is outside the Proof schema; replace the document from markdown to repair it",
    })
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

    expect(
      await screen.findByText(
        "document is outside the Proof schema; replace the document from markdown to repair it"
      )
    ).not.toBeNull();
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
      new ApiError(409, {
        code: "DOC_SCHEMA",
        error:
          "document is outside the Proof schema; replace the document from markdown to repair it",
      })
    );
    act(() => refuse());

    expect(
      await screen.findByText(
        "document is outside the Proof schema; replace the document from markdown to repair it"
      )
    ).not.toBeNull();
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

// Only a stored history that cannot load is rebuilt: the page offers the rebuild for the code the
// server answers that state with, and the read after the rebuild admits the editor again.
test("ProofDocument offers a rebuild for a history that cannot load and reconnects after it", async () => {
  const user = userEvent.setup();
  const getArtifactText = spyOn(api, "getArtifactText").mockRejectedValue(
    new ApiError(409, {
      code: "DOCUMENT_UNLOADABLE",
      error: "the document's stored history cannot load; rebuild it from its latest saved version",
    })
  );
  const rebuildArtifact = spyOn(api, "rebuildArtifact").mockResolvedValue({
    head: 1,
    removed_checkpoints: 0,
    removed_snapshots: 0,
    removed_updates: 3,
    source_version: 1,
    validation_error: "decode live document: unexpected end of input",
  });
  const confirm = spyOn(window, "confirm").mockReturnValue(true);
  const { connections, view } = renderProofDocument();
  try {
    expect(
      await screen.findByText(
        "the document's stored history cannot load; rebuild it from its latest saved version"
      )
    ).not.toBeNull();
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

/** The refusal the document service answers a write with when the document stands so near the
 *  decode cap that only a rebuild lets it take that write (`refuseUndecodable`, in the words it
 *  uses only once the rebuild route's own test answers near): it names the rebuild route of the
 *  document it refused the write for. */
function nearCapRefusal(id: string): ApiError {
  return new ApiError(413, {
    code: "CAP_EXCEEDED",
    error: `document too large to store: this change would leave the document's live state past the 1048576 items ygo decodes in one update, and its history rather than its text holds it there: a person can rebuild the document (POST /api/v1/artifacts/${id}/rebuild, which the document's page offers), which keeps its text and leaves its history small, and the change can then be made again`,
  });
}

/** The same bound's refusal of a document far from the cap - a change so large that it would
 *  carry the document past the cap by itself - which no rebuild mends, so it names none. */
const farFromCapRefusal = new ApiError(413, {
  code: "CAP_EXCEEDED",
  error:
    "document too large to store: this change would leave the document's live state past the 1048576 items ygo decodes in one update; shorten the change, or split the document",
});

/** A write refused with `refusal`, made through the app's one write store as every surface's
 *  write is - the margin's, an ask block's, an upload's - and not by the document itself. */
async function refuseWrite(queryClient: QueryClient, refusal: Error): Promise<void> {
  await act(async () => {
    await writesOf(queryClient)
      .run({
        detail: null,
        inline: true,
        kind: plainWrite,
        label: "Comment on spec.md",
        presentation: "indicator",
        write: () => Promise.reject(refusal),
      })
      .catch(() => undefined);
  });
}

const NEAR_CAP_NOTICE =
  "This document has been edited so much that it must be rebuilt before it can take your change.";

// A write the server refused at the decode cap with words naming this document's rebuild route is
// one only a rebuild lets the document take, and a person who cannot edit the document meets it:
// the page offers that rebuild with the button and the request the editor's own refusal offers,
// closes the editor before it asks, and admits it again once the rebuild has answered.
test("ProofDocument offers the rebuild for a write to it refused near the decode cap", async () => {
  const user = userEvent.setup();
  const queryClient = createQueryClient();
  const { connections, sync, view } = renderProofDocument({ queryClient });
  const closedBeforeTheRebuild: boolean[] = [];
  const rebuildArtifact = spyOn(api, "rebuildArtifact").mockImplementation(async () => {
    closedBeforeTheRebuild.push(connections[0]?.destroyed === true);
    return {
      head: 1,
      removed_checkpoints: 0,
      removed_snapshots: 0,
      removed_updates: 40,
      source_version: 1,
      validation_error:
        "the document's live state holds 1047004 structs, and replacing its text adds 12171 more",
    };
  });
  const confirm = spyOn(window, "confirm").mockReturnValue(true);
  try {
    await sync();
    expect(screen.queryByRole("button", { name: "Rebuild this document" })).not.toBeInTheDocument();

    await refuseWrite(queryClient, nearCapRefusal(artifact.id));
    expect(await screen.findByText(NEAR_CAP_NOTICE)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Rebuild this document" }));
    expect(confirm).toHaveBeenCalledWith(
      "Rebuild this document from its current text? Every browser connected to it, this one included, drops the edits it has not sent yet."
    );
    await waitFor(() => expect(rebuildArtifact).toHaveBeenCalledTimes(1));
    expect(rebuildArtifact.mock.calls[0]).toEqual([artifact.id, undefined, keyedAttempt]);
    expect(closedBeforeTheRebuild).toEqual([true]);
    await waitFor(() => expect(connections).toHaveLength(2));
    expect(screen.queryByText(NEAR_CAP_NOTICE)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Rebuild this document" })).not.toBeInTheDocument();
  } finally {
    view.unmount();
    confirm.mockRestore();
    rebuildArtifact.mockRestore();
  }
});

// The offer goes only where the route takes the document. A refusal of the same bound that names
// no rebuild - a change that would carry a document far from the cap past it - is the change's
// own; one naming another document's rebuild is that document's; and a reading column is not the
// document's page. None of them offers it, and the page's own refusal still does afterwards.
test("ProofDocument offers no rebuild for a refused write the rebuild route would refuse", async () => {
  const queryClient = createQueryClient();
  const pane = renderProofDocument({ queryClient, surface: "pane" });
  try {
    await pane.sync();
    await refuseWrite(queryClient, nearCapRefusal(artifact.id));
    expect(screen.queryByText(NEAR_CAP_NOTICE)).not.toBeInTheDocument();
  } finally {
    pane.view.unmount();
  }

  const { sync, view } = renderProofDocument({ queryClient });
  try {
    await sync();
    await refuseWrite(queryClient, farFromCapRefusal);
    await refuseWrite(queryClient, nearCapRefusal("another-document"));
    expect(screen.queryByText(NEAR_CAP_NOTICE)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Rebuild this document" })).not.toBeInTheDocument();

    await refuseWrite(queryClient, nearCapRefusal(artifact.id));
    expect(
      await screen.findByRole("button", { name: "Rebuild this document" })
    ).toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

const NEAR_CAP_READ_NOTICE =
  "This document has been edited so much that it must be rebuilt before it can take larger changes.";
const NEAR_CAP_REASON =
  "the document's live state holds 1047004 structs, and a write of its whole text, 12171 more, would carry it past the 1048576 ygo decodes in one update";

/** The document's text read as the server answers it, saying whether the rebuild route takes the
 *  document by the decode cap (`near_decode_cap`) and, where it does, why. */
function textRead(nearDecodeCap: boolean): ArtifactText {
  return {
    markdown: "The live document",
    near_decode_cap: nearDecodeCap,
    ...(nearDecodeCap ? { near_decode_cap_reason: NEAR_CAP_REASON } : {}),
    token: "sha256:live-document",
    version: 1,
  };
}

// The person who opens a document someone else's write was refused for - an agent's, from the
// command line - meets no refusal of their own: their typing and their comments still land. The
// document's own read says the rebuild route takes it, so the page offers that rebuild at once,
// with the server's reason, the same button and the same request, and the read after the rebuild
// takes the offer away.
test("ProofDocument offers the rebuild for a document its read finds near the decode cap", async () => {
  const user = userEvent.setup();
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue(textRead(true));
  const { connections, sync, view } = renderProofDocument();
  const closedBeforeTheRebuild: boolean[] = [];
  const rebuildArtifact = spyOn(api, "rebuildArtifact").mockImplementation(async () => {
    closedBeforeTheRebuild.push(connections[0]?.destroyed === true);
    getArtifactText.mockResolvedValue(textRead(false));
    return {
      head: 1,
      removed_checkpoints: 0,
      removed_snapshots: 0,
      removed_updates: 40,
      source_version: 1,
      validation_error: NEAR_CAP_REASON,
    };
  });
  const confirm = spyOn(window, "confirm").mockReturnValue(true);
  try {
    await sync();
    expect(await screen.findByText(NEAR_CAP_READ_NOTICE)).toBeInTheDocument();
    expect(screen.getByText(NEAR_CAP_REASON)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Rebuild this document" }));
    expect(confirm).toHaveBeenCalledWith(
      "Rebuild this document from its current text? Every browser connected to it, this one included, drops the edits it has not sent yet."
    );
    await waitFor(() => expect(rebuildArtifact).toHaveBeenCalledTimes(1));
    expect(rebuildArtifact.mock.calls[0]).toEqual([artifact.id, undefined, keyedAttempt]);
    expect(closedBeforeTheRebuild).toEqual([true]);
    await waitFor(() => expect(connections).toHaveLength(2));
    await waitFor(() => expect(screen.queryByText(NEAR_CAP_READ_NOTICE)).not.toBeInTheDocument());
    expect(screen.queryByRole("button", { name: "Rebuild this document" })).not.toBeInTheDocument();
  } finally {
    view.unmount();
    confirm.mockRestore();
    rebuildArtifact.mockRestore();
    getArtifactText.mockRestore();
  }
});

// The read's answer is the route's own test, so the page offers the rebuild only where it says
// the route takes the document: never for a read that says it does not, never in a reading
// column, and not again once the person has dismissed it.
test("ProofDocument offers no rebuild where its read says the rebuild route would refuse", async () => {
  const user = userEvent.setup();
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue(textRead(false));
  try {
    const far = renderProofDocument();
    await far.sync();
    expect(screen.queryByText(NEAR_CAP_READ_NOTICE)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Rebuild this document" })).not.toBeInTheDocument();
    far.view.unmount();

    getArtifactText.mockResolvedValue(textRead(true));
    const pane = renderProofDocument({ surface: "pane" });
    await pane.sync();
    expect(screen.queryByText(NEAR_CAP_READ_NOTICE)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Rebuild this document" })).not.toBeInTheDocument();
    pane.view.unmount();

    const near = renderProofDocument();
    await near.sync();
    expect(await screen.findByText(NEAR_CAP_READ_NOTICE)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(screen.queryByText(NEAR_CAP_READ_NOTICE)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Rebuild this document" })).not.toBeInTheDocument();
    near.view.unmount();
  } finally {
    getArtifactText.mockRestore();
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

// A reading column (the Inbox's) is not the document's page: it offers no repair and no rebuild,
// because a reader beside a list of questions has not come to mend a document. Each blocked
// admission is reported to the host instead, which shows the question's own card in its place.
test("a pane draws neither prompt for a blocked admission and reports it to its host", async () => {
  const repair = new ApiError(409, {
    code: "DOC_SCHEMA",
    error: "document is outside the Proof schema; replace the document from markdown to repair it",
  });
  const getArtifactText = spyOn(api, "getArtifactText").mockRejectedValue(repair);
  const reported: string[] = [];
  const schema = renderProofDocument({
    onUnavailable: (reason) => reported.push(reason),
    surface: "pane",
  });
  try {
    await waitFor(() =>
      expect(reported).toEqual([
        "document is outside the Proof schema; replace the document from markdown to repair it",
      ])
    );
    expect(
      screen.queryByText("Upload markdown to replace and repair this document.")
    ).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Upload artifact")).not.toBeInTheDocument();
    expect(schema.connections).toHaveLength(0);
  } finally {
    schema.view.unmount();
  }

  getArtifactText.mockRejectedValue(
    new ApiError(409, {
      code: "DOCUMENT_UNLOADABLE",
      error: "the document's stored history cannot load; rebuild it from its latest saved version",
    })
  );
  const history = renderProofDocument({
    onUnavailable: (reason) => reported.push(reason),
    surface: "pane",
  });
  try {
    await waitFor(() => expect(reported).toHaveLength(2));
    expect(reported[1]).toBe(
      "the document's stored history cannot load; rebuild it from its latest saved version"
    );
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
