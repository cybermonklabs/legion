import { afterEach, expect, jest, mock, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";
import { jump, onFakeClock } from "../../__tests__/fake-clock";
import { recordedEditors, selectText, writeAtEnd } from "../../__tests__/recorded-editors";
import { DocumentRuntime } from "../doc/runtime";
import { SAVE_AFTER_MS } from "./InPlaceField";
import { PriorityDescription } from "./PriorityDescription";

const OUTLINE = "# Why\n\n- Every machine runs the daemon\n- Nothing waits on the old build";
/** `OUTLINE` as the editor writes it back: its own bullet, and a closing newline. */
const OUTLINE_READ_BACK =
  "# Why\n\n* Every machine runs the daemon\n* Nothing waits on the old build\n";

const unmounts: (() => void)[] = [];
afterEach(() => {
  for (const unmount of unmounts.splice(0)) unmount();
});

/**
 * The description as a page holds it, in the documents' own editor: `description` is the text
 * the server answered with, which `rerender` moves, and every write lands in `onSave`. Queries
 * are scoped to this render's container, since bun runs every test file in one document.
 */
function described(description: string, canWrite = true) {
  const editors = recordedEditors();
  const onSave = mock((_next: string) => {});
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const page = (text: string, writer: boolean): ReactNode => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <DocumentRuntime.Provider value={editors.runtime}>
          <PriorityDescription
            canWrite={writer}
            description={text}
            label="Description of P1"
            onSave={onSave}
          />
        </DocumentRuntime.Provider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(page(description, canWrite));
  unmounts.push(() => {
    view.unmount();
    client.clear();
  });
  return {
    /** The editor once it holds the description, and its handle. */
    editor: () => editors.named("Description of P1", view.container),
    onSave,
    rerender: (text: string, writer = canWrite) => view.rerender(page(text, writer)),
    scope: within(view.container),
    view,
  };
}

test("the description is the documents' editor, named for it, drawn as the markdown it is", async () => {
  const { editor } = described(`${OUTLINE}\n\nRead [the runbook](https://example.com/rb).`);
  const { box } = await editor();
  // The editor Dispatch's documents are written in, not a field of the page's own.
  expect(box.classList.contains("ProseMirror")).toBe(true);
  expect(box.getAttribute("contenteditable")).toBe("true");
  expect(box.getAttribute("aria-multiline")).toBe("true");
  expect(box.getAttribute("aria-readonly")).toBeNull();
  expect(within(box).getByRole("heading", { name: "Why" })).toBeDefined();
  expect(
    within(box)
      .getAllByRole("listitem")
      .map((item) => item.textContent)
  ).toEqual(["Every machine runs the daemon", "Nothing waits on the old build"]);
  const link = within(box).getByRole("link", { name: "the runbook" });
  expect(link.getAttribute("href")).toBe("https://example.com/rb");
});

test("a selection in it offers no comment, suggestion or ask, and writes no mark", async () => {
  const { editor, onSave, view } = described(OUTLINE);
  const { handle } = await editor();
  handle.view.focus();
  selectText(handle, "Every machine");
  expect(handle.view.state.selection.empty).toBe(false);
  expect(document.querySelector(".dispatch-action-bar")).toBeNull();
  expect(document.querySelector(".mark-popover")).toBeNull();
  for (const name of ["Comment", "Suggest", "Ask"]) {
    expect(within(view.container).queryByRole("button", { name })).toBeNull();
  }
  // Dispatch's Ctrl+Alt+M asks the editor for the same Comment, which it refuses.
  expect(handle.commentOnSelection()).toBe(false);
  act(() => handle.view.dom.blur());
  expect(onSave).not.toHaveBeenCalled();
});

test("an edit is written as markdown once the typing rests, once, with no Save button", async () => {
  const { editor, onSave, view } = described(OUTLINE);
  const { handle } = await editor();
  handle.view.focus();
  await onFakeClock(async () => {
    writeAtEnd(handle, " and is gone", "gone");
    expect(onSave).not.toHaveBeenCalled();
    // One move each, rather than `jump`: Bun's `advanceTimersByTime(0)`, the second round
    // `jump` runs, fires a timer still a millisecond short of its time.
    act(() => jest.advanceTimersByTime(SAVE_AFTER_MS - 1));
    expect(onSave).not.toHaveBeenCalled();
    act(() => jest.advanceTimersByTime(1));
  });
  expect(onSave.mock.calls).toEqual([
    [
      "# Why\n\n* Every machine runs the daemon\n* Nothing waits on the old build and is **gone**\n",
    ],
  ]);
  expect(within(view.container).queryByRole("button", { name: /save/i })).toBeNull();
  // Leaving it writes nothing more: the text was written already.
  act(() => handle.view.dom.blur());
  expect(onSave).toHaveBeenCalledTimes(1);
});

test("leaving the editor writes what it holds at once", async () => {
  const { editor, onSave } = described("Speedruns run again.");
  const { handle } = await editor();
  handle.view.focus();
  writeAtEnd(handle, " More.");
  act(() => handle.view.dom.blur());
  expect(onSave.mock.calls).toEqual([["Speedruns run again. More.\n"]]);
});

test("the editor's own reading of the text is no edit, and writes nothing", async () => {
  const { editor, onSave } = described(OUTLINE);
  const { handle } = await editor();
  expect(handle.getMarkdown()).toBe(OUTLINE_READ_BACK);
  handle.view.focus();
  act(() => handle.view.dom.blur());
  await onFakeClock(() => jump(SAVE_AFTER_MS));
  expect(onSave).not.toHaveBeenCalled();
});

test("closing the editor while a write waits for the typing to rest writes it", async () => {
  const { editor, onSave, view } = described("Speedruns run again.");
  const { handle } = await editor();
  writeAtEnd(handle, " More.");
  view.unmount();
  expect(onSave.mock.calls).toEqual([["Speedruns run again. More.\n"]]);
});

test("the server's text replaces the editor's; under the reader's unwritten change it is shown, and theirs kept beside it until they use it", async () => {
  const { editor, onSave, rerender, scope } = described("Speedruns run again.");
  const { box, handle } = await editor();
  rerender("Written elsewhere.");
  await waitFor(() => expect(box.textContent).toBe("Written elsewhere."));

  handle.view.focus();
  writeAtEnd(handle, " Mine.");
  rerender("Written elsewhere, again.");
  // The editor shows what the server holds, and the reader's text stands beside it.
  expect(box.textContent).toBe("Written elsewhere, again.");
  const notice = scope.getByRole("alert", { name: "Description of P1, changed by another person" });
  expect(notice.textContent).toContain("Another person changed this while you wrote. Your text:");
  expect(within(notice).getByText("Written elsewhere. Mine.")).toBeDefined();
  // Leaving writes nothing: the editor holds the server's text.
  act(() => handle.view.dom.blur());
  expect(onSave).not.toHaveBeenCalled();
  // Using the reader's text puts it back and writes it at once.
  await userEvent.setup().click(within(notice).getByRole("button", { name: "Use my text" }));
  expect(box.textContent).toBe("Written elsewhere. Mine.");
  expect(onSave.mock.calls).toEqual([["Written elsewhere. Mine.\n"]]);
  expect(scope.queryByRole("alert", { name: /changed by another person/ })).toBeNull();
  // The round trip of that write answers with what the editor wrote, which it holds already.
  rerender("Written elsewhere. Mine.\n");
  expect(box.textContent).toBe("Written elsewhere. Mine.");
  expect(onSave).toHaveBeenCalledTimes(1);
});

test("leaving the editor with nothing written takes in the text written elsewhere while the reader was in it", async () => {
  const { editor, onSave, rerender } = described("Speedruns run again.");
  const { box, handle } = await editor();
  handle.view.focus();
  // Another person's write lands while the reader is in the editor; they type nothing, and leave.
  rerender("Written elsewhere.");
  act(() => handle.view.dom.blur());
  expect(onSave).not.toHaveBeenCalled();
  // Nothing of theirs stands now, so the server's text is what the editor shows. Left showing
  // the old text, the reader's next edit writes it back over the other person's, unseen.
  await waitFor(() => expect(box.textContent).toBe("Written elsewhere."));
  handle.view.focus();
  writeAtEnd(handle, " Mine.");
  act(() => handle.view.dom.blur());
  expect(onSave.mock.calls).toEqual([["Written elsewhere. Mine.\n"]]);
});

test("keeping the other person's text lets the reader's go, and writes nothing", async () => {
  const { editor, onSave, rerender, scope } = described("Speedruns run again.");
  const { box, handle } = await editor();
  handle.view.focus();
  writeAtEnd(handle, " Mine.");
  rerender("Written elsewhere.");
  const notice = scope.getByRole("alert", { name: "Description of P1, changed by another person" });
  await userEvent.setup().click(within(notice).getByRole("button", { name: "Keep their text" }));
  expect(scope.queryByRole("alert", { name: /changed by another person/ })).toBeNull();
  expect(box.textContent).toBe("Written elsewhere.");
  // Clicking the button left the editor, and the editor holds the server's text: nothing goes.
  expect(onSave).not.toHaveBeenCalled();
});

test("the page going while a write waits for the typing to rest writes it, once", async () => {
  const { editor, onSave } = described("Speedruns run again.");
  const { handle } = await editor();
  handle.view.focus();
  await onFakeClock(async () => {
    writeAtEnd(handle, " More.");
    expect(onSave).not.toHaveBeenCalled();
    act(() => {
      window.dispatchEvent(new Event("pagehide"));
    });
    expect(onSave.mock.calls).toEqual([["Speedruns run again. More.\n"]]);
    // That was the write waiting for the typing to rest: none goes once the typing has rested.
    await jump(SAVE_AFTER_MS);
  });
  expect(onSave).toHaveBeenCalledTimes(1);
});

test("the keyboard reaches the editor as it reaches any field", async () => {
  const { editor, view } = described(OUTLINE);
  const { box } = await editor();
  const before = view.container.ownerDocument.createElement("button");
  view.container.prepend(before);
  before.focus();
  await userEvent.setup().tab();
  expect(document.activeElement).toBe(box);
});

test("a reader who may not write reads it in the same editor, read-only", async () => {
  const { editor, onSave } = described(OUTLINE, false);
  const { box, handle } = await editor();
  expect(box.classList.contains("ProseMirror")).toBe(true);
  expect(box.getAttribute("contenteditable")).toBe("false");
  expect(box.getAttribute("aria-readonly")).toBe("true");
  expect(within(box).getByRole("heading", { name: "Why" })).toBeDefined();
  expect(handle.view.editable).toBe(false);
  expect(onSave).not.toHaveBeenCalled();
});

test("an empty description says none is written: above the empty editor for a writer, alone for a reader", async () => {
  const writer = described("");
  const { box } = await writer.editor();
  expect(writer.scope.getByText("No description is written. Write one below.")).toBeDefined();
  expect(box.textContent).toBe("");
  writer.view.unmount();

  const reader = described("", false);
  expect(await reader.scope.findByText("No description is written.")).toBeDefined();
  expect(reader.scope.queryByRole("textbox")).toBeNull();
});

test("the server's text in the editor's own reading of the text the edit started from is no change, and leaves the reader's typing", async () => {
  const { editor, onSave, rerender, scope } = described(OUTLINE);
  const { box, handle } = await editor();
  handle.view.focus();
  writeAtEnd(handle, " Mine.");
  // Another writer's editor wrote back the text it read, in the editor's own form: no change of
  // the text, so nothing of the reader's is taken from the editor and kept beside it.
  rerender(OUTLINE_READ_BACK);
  expect(scope.queryByRole("alert", { name: /changed by another person/ })).toBeNull();
  expect(box.textContent).toContain("old build Mine.");
  act(() => handle.view.dom.blur());
  expect(onSave.mock.calls).toEqual([
    ["# Why\n\n* Every machine runs the daemon\n* Nothing waits on the old build Mine.\n"],
  ]);
});
