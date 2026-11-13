import { expect, jest, type Mock, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: the pastes below carry a file, which user-event has no form for
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useState } from "react";

import { realClock, tick } from "../../__tests__/fake-clock";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError } from "../../api/client";
import { SEND_DEADLINE_MS, SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { writesOf } from "../writes/write-store";
import {
  ArtifactDropZone,
  ArtifactUploadRow,
  appendToDraft,
  captureUploadBase,
  DraftUploadStatus,
  ReplacingUploadNote,
  useArtifactUpload,
  useDraftUpload,
} from "./ArtifactUpload";

function TestHarness(): ReactNode {
  const upload = useArtifactUpload({ issue: "CORE-1" });
  return (
    <ArtifactDropZone upload={upload}>
      <ArtifactUploadRow upload={upload} />
    </ArtifactDropZone>
  );
}

/** A draft's field, as a composer holds one: a file pasted into it uploads to the issue, and
 *  its reference is added to the draft (`useDraftUpload`). */
function DraftHarness(): ReactNode {
  const [draft, setDraft] = useState("");
  const upload = useDraftUpload((text) => setDraft((current) => appendToDraft(current, text)));
  return (
    <>
      <div data-testid="draft" onPaste={(event) => upload.paste(event, { issue: "CORE-1" })}>
        {draft}
      </div>
      <DraftUploadStatus upload={upload} />
    </>
  );
}

/** `ui` under a query client of its own, which the cases that stop a late write reach through
 *  the returned `queryClient` as the shell's indicator does. */
function renderWith(ui: ReactNode) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  return {
    ...render(<QueryClientProvider client={queryClient}>{ui}</QueryClientProvider>),
    queryClient,
  };
}

function renderHarness() {
  return renderWith(<TestHarness />);
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
      ref_key: "CORE-1/notes-md",
      session_id: null,
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
    ref_key: "CORE-1/notes-md",
    session_id: null,
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

/** Stages `notes.md` and presses Upload, waiting until `text` has answered the base read, which
 *  the choice of the file starts. It drives the control as a person does (`userEvent`), as every
 *  other case in this file does. */
async function uploadNotes(text: Mock<typeof api.ownerArtifactText>): Promise<void> {
  const user = userEvent.setup();
  const file = new File(["content"], "notes.md", { type: "text/markdown" });
  await user.upload(screen.getByLabelText("Upload artifact"), [file]);
  await screen.findByText("notes.md");
  await waitFor(() => expect(text).toHaveBeenCalledTimes(1));
  await user.click(screen.getByRole("button", { name: "Upload" }));
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

// The window this upload guards runs from the choice of the file to the post: the token is read
// when the person picks what to upload, so a write landing while they type a summary is refused
// rather than written over. A read at the press would guard that press and nothing more.
test("the base is read when the file is chosen, and the press sends that one", async () => {
  const user = userEvent.setup();
  const ownerArtifactText = spyOn(api, "ownerArtifactText")
    .mockResolvedValueOnce({ markdown: "# Notes\n", token: "sha256:chosen", version: 1 })
    .mockResolvedValue({ markdown: "# Notes\n", token: "sha256:moved-on", version: 2 });
  const uploadArtifact = spyOn(api, "uploadArtifact").mockResolvedValue(UPLOADED);
  const view = renderHarness();

  try {
    const file = new File(["content"], "notes.md", { type: "text/markdown" });
    await user.upload(screen.getByLabelText("Upload artifact"), [file]);
    await screen.findByText("notes.md");
    await waitFor(() => expect(ownerArtifactText).toHaveBeenCalledTimes(1));
    expect(uploadArtifact).not.toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: "Upload" }));
    await waitFor(() => expect(uploadArtifact).toHaveBeenCalledTimes(1));
    expect(ownerArtifactText).toHaveBeenCalledTimes(1);
    const [, input] = uploadArtifact.mock.calls[0] ?? [];
    expect(input?.precondition).toEqual({ document: "sha256:chosen" });
  } finally {
    view.unmount();
    uploadArtifact.mockRestore();
    ownerArtifactText.mockRestore();
  }
});

// Dispatch decides a document by the media type alone, as `mime.ParseMediaType` reads it
// (`artifactKind` in the server's `api/artifacts.go`): `text/markdown; charset=utf-8`, which a
// browser does declare, is `text/markdown` there. A page comparing the string the browser gave
// would read no base for it and replace the document naming nothing.
test("a markdown file whose declared type carries a charset names its base", async () => {
  const user = userEvent.setup();
  const ownerArtifactText = spyOn(api, "ownerArtifactText").mockResolvedValue({
    markdown: "# Notes\n",
    token: "sha256:live-notes",
    version: 1,
  });
  const uploadArtifact = spyOn(api, "uploadArtifact").mockResolvedValue(UPLOADED);
  const view = renderHarness();

  try {
    const file = new File(["content"], "notes.md", { type: "text/markdown; charset=utf-8" });
    await user.upload(screen.getByLabelText("Upload artifact"), [file]);
    await screen.findByText("notes.md");
    await waitFor(() => expect(ownerArtifactText).toHaveBeenCalledTimes(1));
    await user.click(screen.getByRole("button", { name: "Upload" }));

    await waitFor(() => expect(uploadArtifact).toHaveBeenCalledTimes(1));
    const [, input] = uploadArtifact.mock.calls[0] ?? [];
    expect(input?.precondition).toEqual({ document: "sha256:live-notes" });
  } finally {
    view.unmount();
    uploadArtifact.mockRestore();
    ownerArtifactText.mockRestore();
  }
});

// Two answers mean there is no base to name, and no others: the owner holds no document under
// that name, and the document's tree is outside the Proof schema, which an upload repairs. A
// read that failed any other way is carried back, because an upload naming no base replaces the
// document, and "the page could not read it" is no reason to do that.
test("only a missing document and a tree outside the Proof schema mean no base to name", async () => {
  const file = new File(["content"], "notes.md", { type: "text/markdown" });
  const owner = { issue: "CORE-1" };
  const text = spyOn(api, "ownerArtifactText");

  try {
    text.mockRejectedValueOnce(
      new ApiError(404, { code: "ARTIFACT_NOT_FOUND", error: "artifact not found" })
    );
    expect(await captureUploadBase(owner, file)).toEqual({ token: undefined });

    text.mockRejectedValueOnce(
      new ApiError(409, {
        code: "DOC_SCHEMA",
        error:
          "document is outside the Proof schema; replace the document from markdown to repair it",
      })
    );
    expect(await captureUploadBase(owner, file)).toEqual({ token: undefined });

    const unreachable = new NetworkError(
      "/api/v1/issues/CORE-1/artifacts/notes.md/text",
      new TypeError("Failed to fetch")
    );
    text.mockRejectedValueOnce(unreachable);
    expect(await captureUploadBase(owner, file)).toEqual({ unreadable: unreachable });

    const lapsed = new ApiError(401, { code: "AUTH_NO_IDENTITY", error: "not signed in" });
    text.mockRejectedValueOnce(lapsed);
    expect(await captureUploadBase(owner, file)).toEqual({ unreadable: lapsed });

    const down = new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" });
    text.mockRejectedValueOnce(down);
    expect(await captureUploadBase(owner, file)).toEqual({ unreadable: down });
  } finally {
    text.mockRestore();
  }
});

// What the row says when that read failed: nothing was uploaded, and what Dispatch said of the
// read, which here names the two documents the filename answers to. The staged file stays, so
// the next press reads the token again.
test("a base read Dispatch refused ends the upload with its own message and posts nothing", async () => {
  const ambiguous =
    '"notes.md" names 2 documents on this issue; use a slug: notes-md (notes.md), ' +
    "notes-md-2 (notes.md)";
  const ownerArtifactText = spyOn(api, "ownerArtifactText").mockRejectedValue(
    new ApiError(400, { code: "ARTIFACT_REFERENCE_AMBIGUOUS", error: ambiguous })
  );
  const uploadArtifact = spyOn(api, "uploadArtifact").mockResolvedValue(UPLOADED);
  const view = renderHarness();

  try {
    await uploadNotes(ownerArtifactText);
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe(
      `Could not read the document this upload would replace, so nothing was uploaded. ${ambiguous}`
    );
    expect(uploadArtifact).not.toHaveBeenCalled();
    expect(screen.getByText("notes.md")).not.toBeNull();
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

// `PRECONDITION_REQUIRED` means this upload named no base, not that the document changed under
// it. The page names one for every document whose token it could read and ends the upload where
// that read failed, so what reaches this is a document created between the page's read and its
// post. Told "this document changed while you were uploading it", a person whose upload was the
// first of that name would be told something untrue.
test("an upload Dispatch required a version of is told it named none, not that the document changed", async () => {
  const ownerArtifactText = spyOn(api, "ownerArtifactText").mockRejectedValue(
    new ApiError(404, { code: "ARTIFACT_NOT_FOUND", error: "artifact not found" })
  );
  const uploadArtifact = spyOn(api, "uploadArtifact").mockRejectedValue(
    // The sentence the server sends when a replacement names no version, which it refuses
    // whatever the deployment (`api/artifacts.go` in the server).
    new ApiError(400, {
      code: "PRECONDITION_REQUIRED",
      error:
        'an upload replacing document "notes.md" must name the token of the version it ' +
        "replaces: read the " +
        "document with `dispatch doc-read`, which prints `Document token: sha256:…`, and " +
        'upload again naming that token as `--precondition-json \'{"document":"sha256:…"}\'`',
    })
  );
  const view = renderHarness();

  try {
    await uploadNotes(ownerArtifactText);
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe(
      "The page could not say which text this upload replaces, and Dispatch requires it. " +
        "Open the document to see what it says now, then upload again."
    );
  } finally {
    view.unmount();
    uploadArtifact.mockRestore();
    ownerArtifactText.mockRestore();
  }
});

// The words of this note live here and are asserted here, once. The Plan tab and the
// Verification tab both render it and their cases ask only that it is there, so this is the
// case that fails when the sentence a person reads changes - including the flag, which is the
// one thing in it a person types.
test("the replacing-upload note says a later upload names the version it replaces, and how", () => {
  const view = render(<ReplacingUploadNote />);

  try {
    expect(screen.getByTestId("replacing-upload-note").textContent).toBe(
      "A later upload under that name replaces the document, so it names the token of the " +
        "version it replaces: add --precondition-json with the document token " +
        "dispatch doc-read prints."
    );
  } finally {
    view.unmount();
  }
});

/**
 * Lets the real event loop turn until `uploadArtifact` has been called `calls` times: until the
 * press's file is kept and its attempt is out. An upload's attempt, and its send deadline with
 * it, start only once its file is kept (`Writes.#start`), and kept files live in
 * `fake-indexeddb` (`__tests__/setup.ts`), which runs its work on the real `setImmediate`: Bun's
 * fake clock holds `setTimeout` and not that, so `tick` moves the clock and not the keep. A case
 * that moved the clock past the deadline before the keep was done found its write still
 * `sending`, with no deadline started, and passed only behind an earlier case on the real clock
 * whose upload had made the database, which leaves a keep few enough turns to finish in those
 * that moving the clock happened to let through. A keep is done here within a few of these
 * turns; one not done after a thousand will not be, and the case fails on what it expects next.
 */
async function untilAttempted(
  uploadArtifact: Mock<typeof api.uploadArtifact>,
  calls: number
): Promise<void> {
  for (let turn = 0; turn < 1_000 && uploadArtifact.mock.calls.length < calls; turn += 1) {
    await act(() => {
      const { promise, resolve } = Promise.withResolvers<void>();
      setImmediate(resolve);
      return promise;
    });
  }
}

// One press is one key, and Dispatch digests the whole body under that key, the `precondition`
// field included (`bodyDigest` in the server's `api/request_keys.go`). An attempt that read the
// base again would send other bytes and be answered `422 KEY_REUSED` instead of what the first
// attempt did - and where the first attempt wrote the version, its answer would be lost. So the
// base is read once for the press: here the first attempt is answered `409 KEY_IN_FLIGHT`, and
// the one after it carries the same base and is answered with the first's landing.
test("a second attempt under one key sends the base the first did and takes the first's answer", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const ownerArtifactText = spyOn(api, "ownerArtifactText")
    .mockResolvedValueOnce({ markdown: "# Notes\n", token: "sha256:chosen", version: 1 })
    .mockResolvedValue({ markdown: "# Notes\n", token: "sha256:moved-on", version: 2 });
  const uploadArtifact = spyOn(api, "uploadArtifact")
    .mockRejectedValueOnce(
      new ApiError(409, { code: "KEY_IN_FLIGHT", error: "the first attempt is running" }, 1_000)
    )
    .mockResolvedValue(UPLOADED);
  const view = renderHarness();

  try {
    const file = new File(["content"], "notes.md", { type: "text/markdown" });
    await user.upload(screen.getByLabelText("Upload artifact"), [file]);
    await tick(0);
    await user.click(screen.getByRole("button", { name: "Upload" }));
    await untilAttempted(uploadArtifact, 1);
    await tick(2_000);

    expect(uploadArtifact).toHaveBeenCalledTimes(2);
    expect(ownerArtifactText).toHaveBeenCalledTimes(1);
    const [first, second] = uploadArtifact.mock.calls;
    expect(first?.[1].precondition).toEqual({ document: "sha256:chosen" });
    expect(second?.[1].precondition).toEqual({ document: "sha256:chosen" });
    expect(second?.[2].key).toBe(first?.[2].key);
    // Answered, so the row let the file go rather than showing a refusal.
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.queryByText("notes.md")).not.toBeInTheDocument();
  } finally {
    view.unmount();
    uploadArtifact.mockRestore();
    ownerArtifactText.mockRestore();
    realClock();
  }
});

// Pressing Upload again after Dispatch's refusal is a press of its own: it reads the token
// again, so it names what the document says now rather than the base that was refused, and goes
// under a key of its own - Dispatch would answer the old key with that refusal for a day. A
// press holding the base read at the choice for ever would be refused for ever.
test("pressing Upload again after a refusal reads the base again, under a key of its own", async () => {
  const user = userEvent.setup();
  const ownerArtifactText = spyOn(api, "ownerArtifactText")
    .mockResolvedValueOnce({ markdown: "# Notes\n", token: "sha256:stale", version: 1 })
    .mockResolvedValue({ markdown: "# Notes\n", token: "sha256:now", version: 2 });
  const uploadArtifact = spyOn(api, "uploadArtifact")
    .mockRejectedValueOnce(
      new ApiError(409, { code: "PRECONDITION_FAILED", error: "document changed" })
    )
    .mockResolvedValue(UPLOADED);
  const view = renderHarness();

  try {
    const file = new File(["content"], "notes.md", { type: "text/markdown" });
    await user.upload(screen.getByLabelText("Upload artifact"), [file]);
    await screen.findByText("notes.md");
    await waitFor(() => expect(ownerArtifactText).toHaveBeenCalledTimes(1));
    await user.click(screen.getByRole("button", { name: "Upload" }));
    await screen.findByRole("alert");

    await user.click(screen.getByRole("button", { name: "Upload" }));
    await waitFor(() => expect(uploadArtifact).toHaveBeenCalledTimes(2));
    expect(ownerArtifactText).toHaveBeenCalledTimes(2);
    const [first, second] = uploadArtifact.mock.calls;
    expect(first?.[1].precondition).toEqual({ document: "sha256:stale" });
    expect(second?.[1].precondition).toEqual({ document: "sha256:now" });
    expect(second?.[2].key).not.toBe(first?.[2].key);
  } finally {
    view.unmount();
    uploadArtifact.mockRestore();
    ownerArtifactText.mockRestore();
  }
});

/** An upload attempt that never reached Dispatch, so the write waits and tries again. */
const unreachable = new NetworkError(
  "/api/v1/issues/CORE-1/artifacts",
  new TypeError("Failed to fetch")
);

/** The press made again is the stopped one: one more attempt, under the first's key, carrying
 *  the base read for that press, which was read once and not since. */
function expectTheStoppedPressAgain(
  uploadArtifact: Mock<typeof api.uploadArtifact>,
  ownerArtifactText: Mock<typeof api.ownerArtifactText>,
  stopped: number
): void {
  expect(uploadArtifact).toHaveBeenCalledTimes(stopped + 1);
  const first = uploadArtifact.mock.calls[0];
  const again = uploadArtifact.mock.calls.at(-1);
  expect(again?.[1].precondition).toEqual({ document: "sha256:chosen" });
  expect(again?.[2].key).toBe(first?.[2].key);
  expect(ownerArtifactText).toHaveBeenCalledTimes(1);
}

// A press the reader stopped is no verdict: it may have reached Dispatch, which answers its key
// with what it did there. So the press made again goes under that key, and the key covers the
// whole body, the `precondition` field included - a press that read the token afresh would read
// what the stopped press wrote, send other bytes under a key of its own, and store a second
// version of the document.
test("pressing Upload again after a stopped press sends that press's base under its key", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const ownerArtifactText = spyOn(api, "ownerArtifactText")
    .mockResolvedValueOnce({ markdown: "# Notes\n", token: "sha256:chosen", version: 1 })
    .mockResolvedValue({ markdown: "# Notes\n", token: "sha256:stored", version: 2 });
  const uploadArtifact = spyOn(api, "uploadArtifact").mockRejectedValue(unreachable);
  const view = renderHarness();

  try {
    const file = new File(["content"], "notes.md", { type: "text/markdown" });
    await user.upload(screen.getByLabelText("Upload artifact"), [file]);
    await tick(0);
    await user.click(screen.getByRole("button", { name: "Upload" }));
    await untilAttempted(uploadArtifact, 1);
    await stopTheLateWrite(view.queryClient);
    expect(screen.getByRole("alert")).not.toBeNull();
    const stopped = uploadArtifact.mock.calls.length;

    uploadArtifact.mockResolvedValue(UPLOADED);
    await user.click(screen.getByRole("button", { name: "Upload" }));
    await untilAttempted(uploadArtifact, stopped + 1);
    await tick(1_000);

    expectTheStoppedPressAgain(uploadArtifact, ownerArtifactText, stopped);
  } finally {
    view.unmount();
    uploadArtifact.mockRestore();
    ownerArtifactText.mockRestore();
    realClock();
  }
});

/** The file one upload attempt carried, for a case that tells two files apart: what the client
 *  takes either carries a file or the text of a document. */
function fileSent(input: Parameters<typeof api.uploadArtifact>[1] | undefined): Blob | undefined {
  return input !== undefined && "file" in input ? input.file : undefined;
}

// A press of another file is a press of its own, however alike the two read: the row reaches a
// stopped press only through the handle the hook gives for the press that sent the staged file,
// and the hook never matches a press by what it carries - the two serialise alike, under one base
// token, and a `File` carries no field `JSON.stringify` writes, so the text alone would make them
// one press. Sent under the stopped press's key, the second file would be answered
// `422 KEY_REUSED` once the first press reached Dispatch, and nothing of it would be stored.
test("a press of another file after a stopped press goes under a key of its own", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  // One token for both reads, so nothing of what the two presses carry tells them apart.
  const ownerArtifactText = spyOn(api, "ownerArtifactText").mockResolvedValue({
    markdown: "# Notes\n",
    token: "sha256:chosen",
    version: 1,
  });
  const uploadArtifact = spyOn(api, "uploadArtifact").mockRejectedValue(unreachable);
  const view = renderHarness();

  try {
    const file = new File(["content"], "notes.md", { type: "text/markdown" });
    const other = new File(["other words"], "notes.md", { type: "text/markdown" });
    expect(JSON.stringify({ base: { token: "sha256:chosen" }, file })).toBe(
      JSON.stringify({ base: { token: "sha256:chosen" }, file: other })
    );

    await user.upload(screen.getByLabelText("Upload artifact"), [file]);
    await tick(0);
    await user.click(screen.getByRole("button", { name: "Upload" }));
    await untilAttempted(uploadArtifact, 1);
    await stopTheLateWrite(view.queryClient);
    const stopped = uploadArtifact.mock.calls.length;

    uploadArtifact.mockResolvedValue(UPLOADED);
    await user.upload(screen.getByLabelText("Upload artifact"), [other]);
    await tick(0);
    await user.click(screen.getByRole("button", { name: "Upload" }));
    await untilAttempted(uploadArtifact, stopped + 1);
    await tick(1_000);

    expect(uploadArtifact).toHaveBeenCalledTimes(stopped + 1);
    const first = uploadArtifact.mock.calls[0];
    const second = uploadArtifact.mock.calls.at(-1);
    expect(fileSent(second?.[1])).toBe(other);
    expect(second?.[2].key).not.toBe(first?.[2].key);
  } finally {
    view.unmount();
    uploadArtifact.mockRestore();
    ownerArtifactText.mockRestore();
    realClock();
  }
});

// The other side of that rule, and what it creates: a press made again from the form is a new
// press. Here the first press was stopped and had reached Dispatch, which stored it; the person
// lets the staged file go, picks the same document again - equal values, a new `File` - and
// presses Upload. That press goes out under a key of its own rather than being answered with
// what the first press did. What keeps it from writing the document a second time is the base:
// it names the token the page read, which the stored press has moved the document past, so
// Dispatch refuses it `409 PRECONDITION_FAILED` and the row says the document changed. The
// person is told, rather than their second press being dropped in silence.
test("a press made again from the form goes out under its own key and is told the document changed", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  // One token for both reads: Dispatch stored the stopped press's upload after the second read,
  // which is why the press that follows names a base the document has left behind.
  const ownerArtifactText = spyOn(api, "ownerArtifactText").mockResolvedValue({
    markdown: "# Notes\n",
    token: "sha256:chosen",
    version: 1,
  });
  const uploadArtifact = spyOn(api, "uploadArtifact").mockRejectedValue(unreachable);
  const view = renderHarness();

  try {
    const file = new File(["content"], "notes.md", { type: "text/markdown" });
    await user.upload(screen.getByLabelText("Upload artifact"), [file]);
    await tick(0);
    await user.click(screen.getByRole("button", { name: "Upload" }));
    await untilAttempted(uploadArtifact, 1);
    await stopTheLateWrite(view.queryClient);
    const stopped = uploadArtifact.mock.calls.length;

    // The staged file is let go and the same document picked afresh, as a person does who takes
    // the stopped press for nothing uploaded.
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    uploadArtifact.mockRejectedValue(
      new ApiError(409, { code: "PRECONDITION_FAILED", error: "document changed" })
    );
    const afresh = new File(["content"], "notes.md", { type: "text/markdown" });
    await user.upload(screen.getByLabelText("Upload artifact"), [afresh]);
    await tick(0);
    await user.click(screen.getByRole("button", { name: "Upload" }));
    await untilAttempted(uploadArtifact, stopped + 1);
    await tick(1_000);

    expect(uploadArtifact).toHaveBeenCalledTimes(stopped + 1);
    const first = uploadArtifact.mock.calls[0];
    const second = uploadArtifact.mock.calls.at(-1);
    expect(fileSent(second?.[1])).toBe(afresh);
    expect(second?.[2].key).not.toBe(first?.[2].key);
    expect(screen.getByRole("alert").textContent).toBe(
      "This document changed while you were uploading it. Open it to see what it says now, " +
        "then upload again."
    );
  } finally {
    view.unmount();
    uploadArtifact.mockRestore();
    ownerArtifactText.mockRestore();
    realClock();
  }
});

// The same rule under a draft's field, where Retry is the control that presses again: the upload
// it makes again is the stopped one, base and all, so Dispatch answers it under that key rather
// than storing the file a second time.
test("a draft's Retry after a stopped upload sends that press's base under its key", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const ownerArtifactText = spyOn(api, "ownerArtifactText")
    .mockResolvedValueOnce({ markdown: "# Notes\n", token: "sha256:chosen", version: 1 })
    .mockResolvedValue({ markdown: "# Notes\n", token: "sha256:stored", version: 2 });
  const uploadArtifact = spyOn(api, "uploadArtifact").mockRejectedValue(unreachable);
  const view = renderWith(<DraftHarness />);

  try {
    const file = new File(["content"], "notes.md", { type: "text/markdown" });
    // user-event's paste carries text, never the file this field uploads.
    fireEvent.paste(screen.getByTestId("draft"), { clipboardData: { files: [file] } });
    // The base is read before the write is made, so the write's own deadline starts here.
    await tick(1_000);
    await stopTheLateWrite(view.queryClient);
    const stopped = uploadArtifact.mock.calls.length;

    uploadArtifact.mockResolvedValue(UPLOADED);
    await user.click(screen.getByRole("button", { name: "Retry" }));
    await tick(1_000);

    expectTheStoppedPressAgain(uploadArtifact, ownerArtifactText, stopped);
  } finally {
    view.unmount();
    uploadArtifact.mockRestore();
    ownerArtifactText.mockRestore();
    realClock();
  }
});

// A press the reader stopped may have stored the file, so the row does not say the upload failed.
// While that file is the staged one, Upload makes the stopped press again (`confirm`), and the
// row says so. Once the staged file is let go, uploading it again is a press of its own, under a
// key of its own, and the row says that instead.
test("an upload the reader stopped says what Upload does, staged and let go", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const ownerArtifactText = spyOn(api, "ownerArtifactText").mockResolvedValue({
    markdown: "# Notes\n",
    token: "sha256:chosen",
    version: 1,
  });
  const uploadArtifact = spyOn(api, "uploadArtifact").mockRejectedValue(unreachable);
  const view = renderHarness();

  try {
    const file = new File(["content"], "notes.md", { type: "text/markdown" });
    await user.upload(screen.getByLabelText("Upload artifact"), [file]);
    await tick(0);
    await user.click(screen.getByRole("button", { name: "Upload" }));
    await untilAttempted(uploadArtifact, 1);
    await stopTheLateWrite(view.queryClient);
    expect(screen.getByRole("alert").textContent).toBe(
      stoppedWriteNote("the upload", "stored", {
        again: "Upload",
        method: "POST",
        stopped: new SendStoppedError(),
      })
    );

    await user.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.getByRole("alert").textContent).toBe(
      stoppedWriteNote("the upload", "stored", {
        anew: "Uploading the file again is a new upload, which Dispatch may store a second time.",
      })
    );
  } finally {
    view.unmount();
    uploadArtifact.mockRestore();
    ownerArtifactText.mockRestore();
    realClock();
  }
});

// Under a draft's field the control is Retry, which makes the stopped press again
// (`DraftUpload.retry`), so the field says what Retry does rather than that the upload failed.
test("a draft's upload the reader stopped says Retry makes it again, not that it failed", async () => {
  jest.useFakeTimers();
  const ownerArtifactText = spyOn(api, "ownerArtifactText").mockResolvedValue({
    markdown: "# Notes\n",
    token: "sha256:chosen",
    version: 1,
  });
  const uploadArtifact = spyOn(api, "uploadArtifact").mockRejectedValue(unreachable);
  const view = renderWith(<DraftHarness />);

  try {
    const file = new File(["content"], "notes.md", { type: "text/markdown" });
    // user-event's paste carries text, never the file this field uploads.
    fireEvent.paste(screen.getByTestId("draft"), { clipboardData: { files: [file] } });
    // The base is read before the write is made, so the write's own deadline starts here.
    await tick(1_000);
    await stopTheLateWrite(view.queryClient);

    // The alert's paragraph, without its Retry button's name.
    expect(screen.getByRole("alert").querySelector("p")?.textContent).toBe(
      stoppedWriteNote("the upload", "stored", {
        again: "Retry",
        method: "POST",
        stopped: new SendStoppedError(),
      })
    );
  } finally {
    view.unmount();
    uploadArtifact.mockRestore();
    ownerArtifactText.mockRestore();
    realClock();
  }
});

// A paste of two files is two presses at once, and the reader can stop both: the hook then holds
// a stopped press for each, under the key that press's attempts went out under. The refusal the
// field shows is its newest press's, whichever press the reader stopped last, so Retry sends the
// file that refusal is about - chart.png here, stopped before notes.md - under chart.png's own
// key, and Dispatch answers it with what that stopped attempt did. Sent as the press stopped
// last instead, notes.md would go out under notes.md's own key, which Dispatch answers as it
// answered that press, so nothing would be stored twice: the harm is that chart.png - the file
// the refusal on screen is about - is never sent, with nothing on screen saying so.
test("a draft's Retry with two presses stopped sends the file whose refusal it shows", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const ownerArtifactText = spyOn(api, "ownerArtifactText").mockResolvedValue({
    markdown: "# Notes\n",
    token: "sha256:chosen",
    version: 1,
  });
  const uploadArtifact = spyOn(api, "uploadArtifact").mockRejectedValue(unreachable);
  const view = renderWith(<DraftHarness />);

  try {
    const notes = new File(["content"], "notes.md", { type: "text/markdown" });
    // A picture replaces no document, so its press names no base: the two presses differ in
    // their text as well as in their files.
    const chart = new File(["bytes"], "chart.png", { type: "image/png" });
    fireEvent.paste(screen.getByTestId("draft"), { clipboardData: { files: [notes, chart] } });
    // Each press's deadline starts once its file is kept, and the second file is kept behind the
    // first, so the window is crossed once for each of them.
    await tick(1_000);
    await tick(SEND_DEADLINE_MS + 1_000);
    await tick(SEND_DEADLINE_MS + 1_000);
    const rows = writesOf(view.queryClient).rows();
    expect(rows.map((row) => row.status)).toEqual(["late", "late"]);
    // chart.png's press is stopped first and notes.md's after it, so the press stopped last is
    // not the press the field's refusal is about.
    act(() => {
      writesOf(view.queryClient).stop(rows[1]?.id ?? 0);
    });
    await tick(60_000);
    act(() => {
      writesOf(view.queryClient).stop(rows[0]?.id ?? 0);
    });
    await tick(60_000);
    const key = uploadArtifact.mock.calls.find((call) => fileSent(call[1]) === chart)?.[2].key;
    const stopped = uploadArtifact.mock.calls.length;

    uploadArtifact.mockResolvedValue(UPLOADED);
    await user.click(screen.getByRole("button", { name: "Retry" }));
    await tick(1_000);

    expect(uploadArtifact).toHaveBeenCalledTimes(stopped + 1);
    const again = uploadArtifact.mock.calls.at(-1);
    expect(fileSent(again?.[1])).toBe(chart);
    expect(again?.[2].key).toBe(key);
    // Body and all: nothing was read for it, so it sends the bytes its key covers, and a picture
    // replaces no document, so those bytes name no version.
    expect(again?.[1].precondition).toBeUndefined();
    expect(ownerArtifactText).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    uploadArtifact.mockRestore();
    ownerArtifactText.mockRestore();
    realClock();
  }
});

// A refusal Dispatch decided ends the press it answered, and a Retry of it is a press of its own
// under a key of its own: Dispatch would answer the old key with that refusal for a day. Here a
// paste of two files is two presses at once, the reader stopped notes.md and Dispatch refused
// chart.png, so the hook holds a stopped press while the refusal on screen is another file's.
// Taken for this refusal, that stopped press would send notes.md again and leave chart.png - the
// file whose refusal is on screen - unsent. Nor does the Retry hand notes.md's key on: that key
// belongs to the press the reader stopped, which only a Retry of that press reaches, by its
// handle, so the same file pasted afresh goes out under a key of its own.
test("a draft's Retry after Dispatch's refusal sends that file and leaves the stopped press alone", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const ownerArtifactText = spyOn(api, "ownerArtifactText").mockResolvedValue({
    markdown: "# Notes\n",
    token: "sha256:chosen",
    version: 1,
  });
  const notes = new File(["content"], "notes.md", { type: "text/markdown" });
  const chart = new File(["bytes"], "chart.png", { type: "image/png" });
  const tooLarge = new ApiError(413, {
    code: "ARTIFACT_TOO_LARGE",
    error: "the file is larger than the 25 MB an upload may be",
  });
  // Nothing of notes.md reaches Dispatch, so its write waits and goes late; chart.png is refused.
  const uploadArtifact = spyOn(api, "uploadArtifact").mockImplementation((_owner, input) =>
    Promise.reject(fileSent(input) === chart ? tooLarge : unreachable)
  );
  const view = renderWith(<DraftHarness />);

  try {
    // user-event's paste carries text, never the files this field uploads.
    fireEvent.paste(screen.getByTestId("draft"), { clipboardData: { files: [notes, chart] } });
    // Each file is kept before its press goes out, so the second press's refusal reaches the
    // field a few turns after the paste.
    await waitFor(() => expect(screen.getByRole("alert").textContent).toContain(tooLarge.message));
    // The write still out is notes.md's, so stopping it leaves the hook holding that press while
    // the refusal the field shows stays chart.png's.
    await stopTheLateWrite(view.queryClient);
    expect(screen.getByRole("alert").textContent).toContain(tooLarge.message);
    const notesKey = uploadArtifact.mock.calls.find((call) => fileSent(call[1]) === notes)?.[2].key;
    const chartKey = uploadArtifact.mock.calls.find((call) => fileSent(call[1]) === chart)?.[2].key;
    const sent = uploadArtifact.mock.calls.length;

    uploadArtifact.mockResolvedValue(UPLOADED);
    await user.click(screen.getByRole("button", { name: "Retry" }));
    await tick(1_000);

    expect(uploadArtifact).toHaveBeenCalledTimes(sent + 1);
    const again = uploadArtifact.mock.calls.at(-1);
    expect(fileSent(again?.[1])).toBe(chart);
    // Dispatch answered chart.png's key with that refusal and would answer it so again, and
    // notes.md's key covers another file altogether, so this press takes a key of its own.
    expect(again?.[2].key).not.toBe(chartKey);
    expect(again?.[2].key).not.toBe(notesKey);

    // The stopped press keeps its key to itself: pasting notes.md again is a later action of the
    // reader's, which builds its own upload, so it is a press of its own under a key of its own
    // rather than the stopped press made again behind their back.
    fireEvent.paste(screen.getByTestId("draft"), { clipboardData: { files: [notes] } });
    await tick(1_000);

    expect(uploadArtifact).toHaveBeenCalledTimes(sent + 2);
    const notesAgain = uploadArtifact.mock.calls.at(-1);
    expect(fileSent(notesAgain?.[1])).toBe(notes);
    expect(notesAgain?.[2].key).not.toBe(notesKey);
    expect(notesAgain?.[2].key).not.toBe(again?.[2].key);
  } finally {
    view.unmount();
    uploadArtifact.mockRestore();
    ownerArtifactText.mockRestore();
    realClock();
  }
});

// Dispatch stored the file and lost its answer (`409 KEY_COMMITTED`): the staged file and its
// summary go, the row says what was stored, and the row's Upload is the picker again, so a second
// press sends nothing.
test("an upload answered KEY_COMMITTED lets go of the staged file, says it was stored, and a second press sends nothing", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
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
    await tick(20);
    expect(uploadArtifact).toHaveBeenCalledTimes(1);
  } finally {
    realClock();
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
