import { expect, jest, type Mock, spyOn, test } from "bun:test";
// biome-ignore lint/style/noRestrictedImports: the pastes below carry a file, which user-event has no form for
import { act, fireEvent, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useState } from "react";

import { realClock, tick } from "../../__tests__/fake-clock";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError } from "../../api/client";
import { SEND_DEADLINE_MS, SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { writesOf } from "../writes/write-store";
import { appendToDraft, DraftUploadStatus, useDraftUpload } from "./ArtifactUpload";
import { renderHarness, renderWith, UPLOADED } from "./artifact-upload-fixture";

// One press, one key: what a press sends when it is attempted again, stopped by the reader and
// made again, or answered by Dispatch under its key, and which presses go out under a key of
// their own instead.

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

/**
 * Lets the real event loop turn until `uploadArtifact` has been called `calls` times: until the
 * press's file is kept and its attempt is out. An upload's attempt, and its send deadline with
 * it, start only once its file is kept (`Writes.#start`), and kept files live in
 * `fake-indexeddb` (`__tests__/setup.ts`), which runs its work on the real `setImmediate`: Bun's
 * fake clock holds `setTimeout` and not that, so `tick` moves the clock and not the keep, and a
 * case that moves the clock past the deadline before the keep is done finds its write still
 * `sending`, with no deadline started. A keep is done here within a few of these turns; one not
 * done after a thousand will not be, and the case fails on what it expects next.
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
