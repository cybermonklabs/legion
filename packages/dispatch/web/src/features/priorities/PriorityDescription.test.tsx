import { afterEach, beforeAll, expect, jest, mock, test } from "bun:test";
import { dispatchWritingBox } from "@legion/contracts";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";
import { BeforeEffects, stepBeforeEffects } from "../../__tests__/before-effects";
import { countEngineParses, displayTree } from "../../__tests__/display-trees";
import { jump, onFakeClock, settle } from "../../__tests__/fake-clock";
import { recordedEditors, selectText, writeAtEnd } from "../../__tests__/recorded-editors";
import type { DisplayTree } from "../../api/types";
import type { CreateEditor } from "../doc/editor";
import { DocumentRuntime } from "../doc/runtime";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import {
  flushTasks,
  installFakeParseWorker,
  restoreParseWorker,
} from "../refs/parse-worker-fixture";
import { SAVE_AFTER_MS } from "./InPlaceField";
import { PriorityDescription } from "./PriorityDescription";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

afterEach(restoreParseWorker);

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
 * the server answered with, `tree` the reading the server sent of it and `exact` whether it said
 * its parser takes the text whole (`description_tree_exact`), which `rerender` moves, and every
 * write lands in `onSave`, with the server's text the editor last took in, unanswered unless a
 * test gives `onSave` answers of its own; `madeOn` holds the text each write's edit was made on
 * (`onSave`'s `on`). `imports` is the length of each text the editor took in as markdown
 * (`setMarkdown`), its own parse of a text, which the browser spec counts as a parse of the page
 * (`e2e/parse-counting.ts`). A text a test puts in `undrawable` is one the editor cannot take in:
 * `setMarkdown` throws for it, as for a text holding what it may not create yet. `beforeEffects`
 * runs a step in the next commit `rerender` makes, before the editor's effects take in what it
 * drew. Queries are scoped to this render's container, since bun runs every test file in one
 * document.
 */
function described(description: string, canWrite = true, tree?: DisplayTree, exact?: boolean) {
  const editors = recordedEditors();
  const imports: number[] = [];
  const undrawable = new Set<string>();
  const createEditor: CreateEditor = async (root, options) => {
    const handle = await editors.runtime.createEditor(root, options);
    const setMarkdown = handle.setMarkdown;
    handle.setMarkdown = (markdown) => {
      if (undrawable.has(markdown)) throw new Error('this editor cannot create node "undrawn" yet');
      imports.push(markdown.length);
      setMarkdown.call(handle, markdown);
    };
    return handle;
  };
  const runtime = { ...editors.runtime, createEditor };
  // Every write is out until a test answers it: the default answer never comes.
  const onSave = mock(
    (_next: string, _from: string): Promise<boolean> => new Promise<boolean>(() => {})
  );
  const madeOn: string[] = [];
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  // What runs in the next commit `rerender` makes: after the editor's render, before its effects
  // take in the text that render drew (`beforeEffects`).
  const { beforeEffects, step } = stepBeforeEffects();
  const page = (
    text: string,
    writer: boolean,
    reading: DisplayTree | undefined,
    readingExact: boolean | undefined
  ): ReactNode => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <DocumentRuntime.Provider value={runtime}>
          <PriorityDescription
            canWrite={writer}
            description={text}
            descriptionTree={reading}
            descriptionTreeExact={readingExact}
            label="Description of P1"
            onSave={(next, from, on) => {
              madeOn.push(on);
              return onSave(next, from);
            }}
          />
          <BeforeEffects step={step} />
        </DocumentRuntime.Provider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(page(description, canWrite, tree, exact));
  unmounts.push(() => {
    view.unmount();
    client.clear();
  });
  return {
    /** Holds each write's answer for the test to give, and answers how it gives one: to the
     *  `at`th write to go, landed or not, in an act that lets the editor follow it. */
    answering: () => {
      const answers: ((landed: boolean) => void)[] = [];
      onSave.mockImplementation(() => {
        const answer = Promise.withResolvers<boolean>();
        answers.push(answer.resolve);
        return answer.promise;
      });
      return (at: number, landed: boolean) =>
        act(async () => {
          answers[at]?.(landed);
          await settle();
        });
    },
    beforeEffects,
    /** The editor once it holds the description, and its handle. */
    editor: () => editors.named("Description of P1", view.container),
    imports,
    madeOn,
    onSave,
    rerender: (text: string, writer = canWrite, reading?: DisplayTree, readingExact?: boolean) =>
      view.rerender(page(text, writer, reading, readingExact)),
    scope: within(view.container),
    undrawable,
    view,
  };
}

test("the description is the documents' editor, named for it, drawn as the markdown it is", async () => {
  const { editor } = described(`${OUTLINE}\n\nRead [the runbook](https://example.com/rb).`);
  const { box } = await editor();
  // The editor Dispatch's documents are written in, not a field of the page's own.
  expect(box.classList.contains("ProseMirror")).toBe(true);
  expect(box).toHaveAttribute("contenteditable", "true");
  expect(box).toHaveAttribute("aria-multiline", "true");
  expect(box).not.toHaveAttribute("aria-readonly");
  expect(within(box).getByRole("heading", { name: "Why" })).toBeDefined();
  expect(
    within(box)
      .getAllByRole("listitem")
      .map((item) => item.textContent)
  ).toEqual(["Every machine runs the daemon", "Nothing waits on the old build"]);
  const link = within(box).getByRole("link", { name: "the runbook" });
  expect(link).toHaveAttribute("href", "https://example.com/rb");
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
      OUTLINE,
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
  expect(onSave.mock.calls).toEqual([["Speedruns run again. More.\n", "Speedruns run again."]]);
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
  expect(onSave.mock.calls).toEqual([["Speedruns run again. More.\n", "Speedruns run again."]]);
});

test("the server's text replaces the editor's; under the reader's unwritten change it is shown, and theirs kept beside it until they use it", async () => {
  const { editor, madeOn, onSave, rerender, scope } = described("Speedruns run again.");
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
  // Using the reader's text puts it back and writes it at once, over the text the editor shows,
  // which it took in with the conflict: no write of theirs is out. The editor never wrote that
  // text, so it is a new edit, made on the text the editor took in.
  await userEvent.setup().click(within(notice).getByRole("button", { name: "Use my text" }));
  expect(box.textContent).toBe("Written elsewhere. Mine.");
  expect(onSave).toHaveBeenCalledTimes(1);
  expect(onSave).toHaveBeenLastCalledWith(
    "Written elsewhere. Mine.\n",
    "Written elsewhere, again."
  );
  expect(madeOn).toEqual(["Written elsewhere, again.\n"]);
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
  expect(onSave.mock.calls).toEqual([["Written elsewhere. Mine.\n", "Written elsewhere."]]);
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

// The page's answer to a write is that write's own, never another write's of the same text. The
// reader's text is out when another person's arrives; Use my text writes it again, and the reader
// writes on. Then the first write is refused, as another person's text stood: Use my text's write
// is still out, and its landing is the reader's own text coming back, with no notice and their
// writing left alone.
test("the answer to an earlier write of the same text leaves Use my text's write out, and that write's landing is the reader's own", async () => {
  const { answering, editor, onSave, rerender, scope } = described("Speedruns run again.");
  const answer = answering();
  const { box, handle } = await editor();
  handle.view.focus();
  writeAtEnd(handle, " Mine.");
  act(() => handle.view.dom.blur());
  rerender("Written elsewhere.");
  const notice = scope.getByRole("alert", { name: "Description of P1, changed by another person" });
  await userEvent.setup().click(within(notice).getByRole("button", { name: "Use my text" }));
  handle.view.focus();
  writeAtEnd(handle, " More.");
  act(() => handle.view.dom.blur());
  expect(onSave.mock.calls.map(([next]) => next)).toEqual([
    "Speedruns run again. Mine.\n",
    "Speedruns run again. Mine.\n",
    "Speedruns run again. Mine. More.\n",
  ]);
  await answer(0, false);
  rerender("Speedruns run again. Mine.\n");
  expect(scope.queryByRole("alert", { name: /changed by another person/ })).toBeNull();
  expect(box.textContent).toBe("Speedruns run again. Mine. More.");
});

// Another person's text the editor cannot draw arrives over the reader's unwritten change, and
// then a second one it cannot draw either: no editor is drawn for either, and the reader's text
// stands beside the line that says so. Use my text writes it over the text the server holds now,
// the second; over the first, Dispatch would refuse it each time it is used. No editor holds
// anything of the reader's then, so the write is made on that text too.
test("Use my text with no editor drawn names the newest text the server sent, though the editor could draw neither", async () => {
  const { editor, madeOn, onSave, rerender, scope, undrawable } = described("Speedruns run again.");
  undrawable.add("Written elsewhere.").add("Written elsewhere, again.");
  const { handle } = await editor();
  handle.view.focus();
  writeAtEnd(handle, " Mine.");
  const failed = () => scope.queryByText(/This description could not be drawn/);
  rerender("Written elsewhere.");
  await waitFor(() => expect(failed()).not.toBeNull());
  rerender("Written elsewhere, again.");
  expect(failed()).toBeNull();
  await waitFor(() => expect(failed()).not.toBeNull());
  expect(scope.queryByRole("textbox", { name: "Description of P1" })).toBeNull();
  const notice = scope.getByRole("alert", { name: "Description of P1, changed by another person" });
  await userEvent.setup().click(within(notice).getByRole("button", { name: "Use my text" }));
  expect(onSave.mock.calls).toEqual([
    ["Speedruns run again. Mine.\n", "Written elsewhere, again."],
  ]);
  expect(madeOn).toEqual(["Written elsewhere, again."]);
});

// Each write hands on the text the editor held as its edit began (`onSave`'s `on`), in the
// form the editor writes, so a write made on the editor's own earlier one names that one's text:
// the text it opened on, as it reads it back; its own last write, though that did not land; the
// other person's text it took in; and for Use my text that writes the editor's last write again,
// the text that write was made on, since it is that same edit.
test("each write hands on the text the editor held as its edit began, in the form it writes", async () => {
  const { answering, editor, madeOn, onSave, rerender, scope } = described("Speedruns run again.");
  const answer = answering();
  const { handle } = await editor();
  const writeAndLeave = (text: string) => {
    handle.view.focus();
    writeAtEnd(handle, text);
    act(() => handle.view.dom.blur());
  };
  writeAndLeave(" More.");
  await answer(0, false);
  writeAndLeave(" Again.");
  await answer(1, false);
  rerender("Written elsewhere.");
  writeAndLeave(" Mine.");
  rerender("Bob.");
  const notice = scope.getByRole("alert", { name: "Description of P1, changed by another person" });
  await userEvent.setup().click(within(notice).getByRole("button", { name: "Use my text" }));
  expect(onSave.mock.calls.map(([written]) => written)).toEqual([
    "Speedruns run again. More.\n",
    "Speedruns run again. More. Again.\n",
    "Written elsewhere. Mine.\n",
    "Written elsewhere. Mine.\n",
  ]);
  expect(madeOn).toEqual([
    "Speedruns run again.\n",
    "Speedruns run again. More.\n",
    "Written elsewhere.\n",
    "Written elsewhere.\n",
  ]);
});

// The text an edit was made on is the editor's own, apart from the text it compares what it holds
// with (`base`), which a read moves under typing the reader has not written. " A", made on the
// text the editor opened on, lands, and the read that shows it is slow; " B", made on " A", ends
// without landing (she stopped it). She types on, and the read answers " A" while her typing
// waits: the editor compares with " A" from then on, but her typing was made on " B", and its
// write says so.
test("a read that answers an earlier write while the reader's typing waits leaves that typing made on the text it began on", async () => {
  const { answering, editor, madeOn, onSave, rerender } = described("Speedruns run again.");
  const answer = answering();
  const { handle } = await editor();
  handle.view.focus();
  writeAtEnd(handle, " A");
  act(() => handle.view.dom.blur());
  await answer(0, true);
  handle.view.focus();
  writeAtEnd(handle, " B");
  act(() => handle.view.dom.blur());
  await answer(1, false);
  handle.view.focus();
  writeAtEnd(handle, " C");
  rerender("Speedruns run again. A\n");
  act(() => handle.view.dom.blur());
  expect(onSave.mock.calls.map(([written]) => written)).toEqual([
    "Speedruns run again. A\n",
    "Speedruns run again. A B\n",
    "Speedruns run again. A B C\n",
  ]);
  expect(madeOn).toEqual([
    "Speedruns run again.\n",
    "Speedruns run again. A\n",
    "Speedruns run again. A B\n",
  ]);
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
    expect(onSave.mock.calls).toEqual([["Speedruns run again. More.\n", "Speedruns run again."]]);
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
  expect(box).toHaveFocus();
});

test("a reader who may not write reads it in the same editor, read-only", async () => {
  const { editor, onSave } = described(OUTLINE, false);
  const { box, handle } = await editor();
  expect(box.classList.contains("ProseMirror")).toBe(true);
  expect(box).toHaveAttribute("contenteditable", "false");
  expect(box).toHaveAttribute("aria-readonly", "true");
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
  // the text, so nothing of the reader's is taken from the editor and kept beside it. The server
  // holds that form now, so the reader's write names it.
  rerender(OUTLINE_READ_BACK);
  expect(scope.queryByRole("alert", { name: /changed by another person/ })).toBeNull();
  expect(box.textContent).toContain("old build Mine.");
  act(() => handle.view.dom.blur());
  expect(onSave.mock.calls).toEqual([
    [
      "# Why\n\n* Every machine runs the daemon\n* Nothing waits on the old build Mine.\n",
      OUTLINE_READ_BACK,
    ],
  ]);
});

/** Emphasis nested 4,000 deep, 24,001 characters: the shape micromark spends 672,095,992
 *  `attention` steps on, about 55 s (cybermonklabs/legion#4437). */
const NESTED = `${"*a ".repeat(4_000)}b${" c*".repeat(4_000)}`;

/** The server's reading of `NESTED`, as Go's display read writes it for marks nested past its
 *  bound (pmdoc's `maxInlineNesting`, `literalDocument`): one paragraph of the text's own
 *  characters, no mark among them. It is built by hand (`asText`) because the page's own engine,
 *  which `displayTree` reads with, is the parser this test keeps the page from running. */
const NESTED_TREE = asText(NESTED);

/** The line under a description drawn from the server's reading where its parser does not take
 *  the description whole. */
const NOT_MARKDOWN =
  "Dispatch cannot read this description as markdown, so it cannot be edited on this page.";

/** A description of a heading, a list and stars, which the server's parser takes whole. */
const PLAN = "# Why\n\n- first host\n- second host\n\nShip **every** host, *then* check.";

/** `PLAN` with one word written at its end, as the editor writes it back from its markdown: its
 *  own bullet, a closing newline, and not one character escaped. */
const PLAN_WRITTEN =
  "# Why\n\n* first host\n* second host\n\nShip **every** host, *then* check. Done.\n";

/** `markdown` as a reading of the server's that fell back draws it (pmdoc's `literalDocument`): a
 *  paragraph of its own characters for each part between blank lines, a hard break between its
 *  lines. */
function asText(markdown: string): DisplayTree {
  const paragraph = (part: string) => ({
    content: part
      .split("\n")
      .flatMap((line, at) => [
        ...(at === 0 ? [] : [{ attrs: { isInline: false }, type: "hardbreak" }]),
        { text: line, type: "text" },
      ]),
    type: "paragraph",
  });
  return { content: markdown.trimEnd().split("\n\n").map(paragraph), type: "doc" };
}

/** `PLAN` as a reading of the server's that fell back draws it (`asText`). An editor that took it
 *  in and wrote it back would write `\# Why` for `# Why`. */
const PLAN_AS_TEXT = asText(PLAN);

/** Another person's text, which the server cannot read as markdown either. */
const ELSEWHERE = "# Why\n\n- first host\n\nShip one host first.";

/** That the editor draws the reading of `markdown` (`asText`) - its lines run together, since
 *  neither a paragraph's end nor a hard break is a character of it - read-only, and described
 *  by the line that says why. */
function drawsReading(box: HTMLElement, markdown: string): void {
  expect(box.textContent).toBe(markdown.trimEnd().split(/\n+/).join(""));
  expect(within(box).queryByRole("heading")).toBeNull();
  expect(box).toHaveAttribute("contenteditable", "false");
  const why = box.getAttribute("aria-describedby");
  expect(why === null ? null : document.getElementById(why)?.textContent).toBe(NOT_MARKDOWN);
}

test.each([
  ["a reader who may not write it", false],
  ["a person who may write it", true],
] as const)("a description the server cannot read as markdown is drawn from its tree for %s, read-only, with the line that says why, and nothing parses it or is written from it", async (_reader, canWrite) => {
  const worker = installFakeParseWorker();
  const parses = countEngineParses();
  try {
    const { editor, imports, onSave, scope, view } = described(
      NESTED,
      canWrite,
      NESTED_TREE,
      false
    );
    // The editor builds the tree it is given with no parser of its own: its markdown taken in
    // instead is the editor's parse. It is drawn on the real clock, as every other row of this
    // file draws it: the cold lane holds a lazily loaded module for a second of real time
    // (`hold-lazy-loads.ts`, a child `sleep`), and a `findBy*` on a fake clock spends its wait on
    // fake time, so it gives up before the module arrives (seed 1454903779 of the cold lane).
    const { box, handle } = await editor();
    expect(box).toHaveAttribute("contenteditable", "false");
    expect(box).toHaveAttribute("aria-readonly", "true");
    expect(box.textContent).toBe(NESTED);
    expect(box.querySelector("em, strong")).toBeNull();
    const why = scope.getByText(NOT_MARKDOWN);
    expect(box).toHaveAttribute("aria-describedby", why.id);
    // On the fake clock from here, so the stand-in's zero-delay timers, and any the engine sets,
    // run in `flushTasks` and wait no real time: a body sent a task late still shows in the reads
    // below.
    await onFakeClock(async () => {
      await act(() => flushTasks());
      expect(worker.requests.map((request) => request.markdown.length)).toEqual([]);
      expect(parses.count()).toBe(0);
      expect(imports).toEqual([]);
      // Nothing is written from that reading. A read-only editor takes no focus (ProseMirror
      // focuses an editable view alone), so no blur reaches it; what can is a change that
      // reaches its document once the typing rests, the page going, and the editor closing.
      writeAtEnd(handle, " x");
      await jump(SAVE_AFTER_MS);
    });
    act(() => {
      window.dispatchEvent(new Event("pagehide"));
    });
    view.unmount();
    expect(onSave).not.toHaveBeenCalled();
  } finally {
    parses.restore();
  }
});

test("another person's text the server cannot read as markdown arrives in a writer's editor from its tree, read-only from then on, and nothing parses it or is written from it", async () => {
  const stored = "Speedruns run again.";
  const { editor, imports, onSave, rerender, scope } = described(
    stored,
    true,
    displayTree(stored),
    true
  );
  const { box, handle } = await editor();
  expect(box).toHaveAttribute("contenteditable", "true");
  const parses = countEngineParses();
  try {
    // The writer is in the editor as another person writes the nested text, and the server
    // answers with its reading of it, and says its parser does not take the text whole.
    handle.view.focus();
    rerender(NESTED, true, NESTED_TREE, false);
    await waitFor(() => expect(box.textContent).toBe(NESTED));
    expect(box).toHaveAttribute("contenteditable", "false");
    expect(box).toHaveAttribute("aria-readonly", "true");
    expect(scope.getByText(NOT_MARKDOWN)).toBeDefined();
    expect(box.querySelector("em, strong")).toBeNull();
    expect(parses.count()).toBe(0);
    // Only the text the editor opened with, which the server's parser takes whole, went in as
    // markdown.
    expect(imports).toEqual([stored.length]);
    // A change that reaches the editor's document now, and the writer leaving it, write nothing.
    writeAtEnd(handle, " x");
    act(() => handle.view.dom.blur());
    expect(onSave).not.toHaveBeenCalled();
  } finally {
    parses.restore();
  }
});

test("a writer's editor takes in, as its markdown, a description the server's parser takes whole, and a one-word edit writes its heading, its list and its stars back with nothing escaped", async () => {
  const { editor, imports, onSave, scope } = described(PLAN, true, displayTree(PLAN), true);
  const { box, handle } = await editor();
  expect(box).toHaveAttribute("contenteditable", "true");
  // The markdown and not the tree: the tree is a thing to draw, which gives every attribute the
  // server owns its default.
  expect(imports).toEqual([PLAN.length]);
  expect(within(box).getByRole("heading", { name: "Why" })).toBeDefined();
  expect(scope.queryByText(NOT_MARKDOWN)).toBeNull();
  handle.view.focus();
  writeAtEnd(handle, " Done.");
  act(() => handle.view.dom.blur());
  expect(onSave.mock.calls).toEqual([[PLAN_WRITTEN, PLAN]]);
});

test("an answer that says nothing of its reading, a pod's of the build before this one, fills a writer's editor from the markdown and a reader's who may not write from the tree", async () => {
  // The reading is one that fell back, which such a pod sends with no word on it.
  const writer = described(PLAN, true, PLAN_AS_TEXT);
  const { box, handle } = await writer.editor();
  expect(box).toHaveAttribute("contenteditable", "true");
  expect(writer.imports).toEqual([PLAN.length]);
  expect(within(box).getByRole("heading", { name: "Why" })).toBeDefined();
  expect(writer.scope.queryByText(NOT_MARKDOWN)).toBeNull();
  handle.view.focus();
  writeAtEnd(handle, " Done.");
  act(() => handle.view.dom.blur());
  expect(writer.onSave.mock.calls).toEqual([[PLAN_WRITTEN, PLAN]]);
  writer.view.unmount();

  const reader = described(PLAN, false, PLAN_AS_TEXT);
  const { box: read } = await reader.editor();
  expect(read).toHaveAttribute("contenteditable", "false");
  expect(reader.imports).toEqual([]);
  expect(within(read).queryByRole("heading")).toBeNull();
  expect(read.textContent).toContain("# Why");
  expect(reader.scope.queryByText(NOT_MARKDOWN)).toBeNull();
});

test("an editor drawn from the reading of a text the server said it cannot read as markdown takes the text in as markdown once an answer that says nothing of it lets the writer write, so no keystroke writes the reading", async () => {
  const { editor, imports, onSave, rerender, scope } = described(PLAN, true, PLAN_AS_TEXT, false);
  const { box, handle } = await editor();
  expect(box).toHaveAttribute("contenteditable", "false");
  expect(imports).toEqual([]);
  // A pod of the build before this one answers the same text and reading during a rolling
  // deploy, and says nothing of the reading.
  rerender(PLAN, true, PLAN_AS_TEXT, undefined);
  await waitFor(() => expect(box).toHaveAttribute("contenteditable", "true"));
  expect(imports).toEqual([PLAN.length]);
  expect(within(box).getByRole("heading", { name: "Why" })).toBeDefined();
  expect(scope.queryByText(NOT_MARKDOWN)).toBeNull();
  handle.view.focus();
  writeAtEnd(handle, " Done.");
  act(() => handle.view.dom.blur());
  expect(onSave.mock.calls).toEqual([[PLAN_WRITTEN, PLAN]]);
});

test("a description the reader has just written is drawn from its own text, never from the tree of the stored one", async () => {
  const stored = "The **old** plan.";
  const { editor, onSave } = described(stored, true, displayTree(stored), true);
  const { box, handle } = await editor();
  expect(within(box).getByText("old").tagName).toBe("STRONG");
  // The reader writes "The *new* plan.": "new" over "old", in the editor's own emphasis, through
  // the editor's own transaction, since happy-dom delivers no keystroke to ProseMirror.
  handle.view.focus();
  selectText(handle, "old");
  const { view } = handle;
  const emphasis = view.state.schema.marks.emphasis;
  if (emphasis === undefined) throw new Error("the editor has no emphasis mark");
  view.dispatch(
    view.state.tr.replaceSelectionWith(view.state.schema.text("new", [emphasis.create()]), false)
  );
  act(() => handle.view.dom.blur());
  expect(onSave.mock.calls).toEqual([["The *new* plan.\n", stored]]);
  // The server still answers the old text and its tree, and the editor shows what was written.
  expect(within(box).getByText("new").tagName).toBe("EM");
  expect(within(box).queryByText("old")).toBeNull();
});

test("a writer's write answered after the server said it cannot read the old text as markdown leaves the editor on the answer's reading, and no reading is ever offered or written as the writer's text", async () => {
  const { editor, onSave, rerender, scope, view } = described(PLAN, true, displayTree(PLAN), true);
  const { box, handle } = await editor();
  handle.view.focus();
  await onFakeClock(async () => {
    // The writer types, and the typing at rest writes it.
    writeAtEnd(handle, " Done.");
    await jump(SAVE_AFTER_MS);
    expect(onSave.mock.calls).toEqual([[PLAN_WRITTEN, PLAN]]);
    // Before the write's answer, the stored text comes back from a server that says it cannot
    // read it as markdown: a pod of another build answering in turn during a rolling deploy.
    // Then the write's answer comes, with the same word on it, and then another person's text.
    rerender(PLAN, true, PLAN_AS_TEXT, false);
    rerender(PLAN_WRITTEN, true, asText(PLAN_WRITTEN), false);
    rerender(ELSEWHERE, true, asText(ELSEWHERE), false);
    // The writer uses what the page offers as their text, if it offers any. The write was sent and
    // answered, so nothing of theirs is left to offer: a reading offered as theirs and put back
    // would write `\# Why` for `# Why`.
    const offered = scope.queryByRole("alert", { name: /changed by another person/ });
    if (offered !== null) {
      act(() => within(offered).getByRole("button", { name: "Use my text" }).click());
    }
    expect(onSave.mock.calls.flat().filter((written) => /\\[#*-]/.test(written))).toEqual([]);
    expect(onSave.mock.calls).toEqual([[PLAN_WRITTEN, PLAN]]);
    expect(offered).toBeNull();
    // The editor draws the latest answer's reading, never the stale one, and writes nothing from
    // it as the writer leaves it, the page goes, or the editor closes.
    drawsReading(box, ELSEWHERE);
    act(() => handle.view.dom.blur());
    act(() => {
      window.dispatchEvent(new Event("pagehide"));
    });
  });
  view.unmount();
  expect(onSave).toHaveBeenCalledTimes(1);
});

test("a writer's editor draws the reading of a text the server cannot read as markdown only once the typing in it is written and answered, and writes that typing once, as typed", async () => {
  const { editor, onSave, rerender } = described(PLAN, true, displayTree(PLAN), true);
  const { box, handle } = await editor();
  handle.view.focus();
  await onFakeClock(async () => {
    writeAtEnd(handle, " Done.");
    // The server says it cannot read the stored text as markdown while the typing waits to be
    // written: the editor keeps the typing in the markdown it was made in.
    rerender(PLAN, true, PLAN_AS_TEXT, false);
    expect(within(box).getByRole("heading", { name: "Why" })).toBeDefined();
    expect(box.textContent).toContain("check. Done.");
    expect(onSave).not.toHaveBeenCalled();
    // The typing at rest is written as typed, and its answer says the same of it.
    await jump(SAVE_AFTER_MS);
    expect(onSave.mock.calls).toEqual([[PLAN_WRITTEN, PLAN]]);
    rerender(PLAN_WRITTEN, true, asText(PLAN_WRITTEN), false);
    drawsReading(box, PLAN_WRITTEN);
    act(() => handle.view.dom.blur());
    await jump(SAVE_AFTER_MS);
  });
  expect(onSave).toHaveBeenCalledTimes(1);
});

// INFRA-2947 tester, review round 6's second finding: the server says it cannot read the stored
// text as markdown while a write of the writer's is out, so the editor keeps the typing in the
// markdown it was made in. The write then ends without landing - refused, or stopped - and no
// answer of the server's will bring its text back: the editor draws the stored text's reading at
// once, rather than holding the writer's markdown until some other text arrives.
test("a writer's write that ends without landing, after the server said it cannot read the stored text as markdown, leaves the editor on that text's reading", async () => {
  const { editor, onSave, rerender } = described(PLAN, true, displayTree(PLAN), true);
  const answer = Promise.withResolvers<boolean>();
  onSave.mockImplementation(() => answer.promise);
  const { box, handle } = await editor();
  handle.view.focus();
  await onFakeClock(async () => {
    writeAtEnd(handle, " Done.");
    await jump(SAVE_AFTER_MS);
    expect(onSave.mock.calls).toEqual([[PLAN_WRITTEN, PLAN]]);
    rerender(PLAN, true, PLAN_AS_TEXT, false);
    expect(box.textContent).toContain("check. Done.");
    await act(async () => {
      answer.resolve(false);
      await answer.promise;
    });
    drawsReading(box, PLAN);
    act(() => handle.view.dom.blur());
    await jump(SAVE_AFTER_MS);
  });
  expect(onSave).toHaveBeenCalledTimes(1);
});

test("a writer's write answered after the editor drew another person's reading is taken in, and the text kept beside the editor as theirs is the one they wrote, never a reading", async () => {
  const { editor, onSave, rerender, scope } = described(PLAN, true, displayTree(PLAN), true);
  const { box, handle } = await editor();
  handle.view.focus();
  await onFakeClock(async () => {
    writeAtEnd(handle, " Done.");
    await jump(SAVE_AFTER_MS);
    expect(onSave.mock.calls).toEqual([[PLAN_WRITTEN, PLAN]]);
    // Before the write's answer, the server says it cannot read the stored text as markdown, and
    // then another person's text comes, which it cannot read either: the editor draws that reading.
    rerender(PLAN, true, PLAN_AS_TEXT, false);
    rerender(ELSEWHERE, true, asText(ELSEWHERE), false);
    drawsReading(box, ELSEWHERE);
    // The write is still out, so the writer's text is kept beside the editor: the text they wrote.
    const notice = scope.getByRole("alert", {
      name: "Description of P1, changed by another person",
    });
    expect(notice.textContent).toContain(PLAN_WRITTEN);
    expect(notice.textContent).not.toMatch(/\\[#*-]/);
    // The write's answer lands after that reading was drawn: it is the server's text now, and the
    // editor draws its reading. The server holds the writer's text, so none is kept.
    rerender(PLAN_WRITTEN, true, asText(PLAN_WRITTEN), false);
    drawsReading(box, PLAN_WRITTEN);
    expect(scope.queryByRole("alert", { name: /changed by another person/ })).toBeNull();
  });
  expect(onSave.mock.calls).toEqual([[PLAN_WRITTEN, PLAN]]);
});

// The editor holds the server's reading of another person's text, read-only, with the writer's
// text kept beside it, and they use it: `putBack`'s branch for an editor the reader does not write
// in. Their kept text is written as it is, over the text the editor took in, and nothing drawn
// from the reading is: the editor does not take the kept text in, and its own write (`write`) is
// never made. The kept text is the writer's own write again, so it is made on what that was.
test("Use my text beside an editor the reader does not write in, drawn from the server's reading, writes the kept text as it is, and nothing drawn from the reading", async () => {
  const { editor, madeOn, onSave, rerender, scope } = described(
    PLAN,
    true,
    displayTree(PLAN),
    true
  );
  const { box, handle } = await editor();
  handle.view.focus();
  await onFakeClock(async () => {
    writeAtEnd(handle, " Done.");
    await jump(SAVE_AFTER_MS);
    rerender(PLAN, true, PLAN_AS_TEXT, false);
    rerender(ELSEWHERE, true, asText(ELSEWHERE), false);
    drawsReading(box, ELSEWHERE);
  });
  const reading = handle.getMarkdown();
  const notice = scope.getByRole("alert", { name: "Description of P1, changed by another person" });
  act(() => within(notice).getByRole("button", { name: "Use my text" }).click());
  expect(onSave.mock.calls.map(([written]) => written)).not.toContain(reading);
  expect(onSave.mock.calls.flat().filter((written) => /\\[#*-]/.test(written))).toEqual([]);
  expect(onSave.mock.calls).toEqual([
    [PLAN_WRITTEN, PLAN],
    [PLAN_WRITTEN, ELSEWHERE],
  ]);
  const [first] = madeOn;
  expect(madeOn).toEqual([first, first]);
});

// No reader's sequence changes an editor that holds the server's reading: it is read-only, and it
// holds one only while nothing of the writer's is out or unwritten. A change made through its view
// stands for any that would. The editor's one write (`write`), which every write of what it holds
// goes through, writes nothing while it holds a reading: not as the writer leaves it, the typing
// rests, the page goes, or the editor closes.
test("a change to an editor that holds the server's reading is never written, whatever made it", async () => {
  const { editor, onSave, rerender, view } = described(PLAN, true, displayTree(PLAN), true);
  const { box, handle } = await editor();
  handle.view.focus();
  rerender(ELSEWHERE, true, asText(ELSEWHERE), false);
  drawsReading(box, ELSEWHERE);
  await onFakeClock(async () => {
    writeAtEnd(handle, " Done.");
    expect(handle.getMarkdown()).toContain("Done.");
    act(() => handle.view.dom.blur());
    await jump(SAVE_AFTER_MS);
    act(() => {
      window.dispatchEvent(new Event("pagehide"));
    });
  });
  view.unmount();
  expect(onSave).not.toHaveBeenCalled();
});

// The page draws another person's text while the writer's typing waits, and a write goes before
// the editor's effects take that text in: the typing at rest, as here, or the editor left as the
// same commit turns it read-only for a text the server's parser does not take whole. The write
// names the text the editor started from, as the server stores it - never the page's read, which
// holds the other person's text by then - so Dispatch refuses it rather than writing the writer's
// edit over theirs, and the editor takes their text in and keeps the writer's beside it, as it
// does where their text arrives first.
test.each([
  ["one the server's parser takes whole", true],
  ["one the server's parser does not take whole", false],
] as const)("a write made once the page drew another person's text, %s, and before the editor took it in, names the text the editor started from", async (_text, exact) => {
  const { beforeEffects, editor, onSave, rerender, scope } = described(
    PLAN,
    true,
    displayTree(PLAN),
    true
  );
  const { box, handle } = await editor();
  handle.view.focus();
  await onFakeClock(async () => {
    writeAtEnd(handle, " Done.");
    beforeEffects(() => jest.advanceTimersByTime(SAVE_AFTER_MS));
    rerender(ELSEWHERE, true, exact ? displayTree(ELSEWHERE) : asText(ELSEWHERE), exact);
    await jump(SAVE_AFTER_MS);
  });
  expect(onSave.mock.calls).toEqual([[PLAN_WRITTEN, PLAN]]);
  expect(box.textContent).toContain("Ship one host first.");
  expect(box.textContent).not.toContain("Done.");
  const notice = scope.getByRole("alert", { name: "Description of P1, changed by another person" });
  expect(notice.textContent).toContain(PLAN_WRITTEN);
  // Nothing more is written: the editor holds the other person's text.
  act(() => handle.view.dom.blur());
  expect(onSave).toHaveBeenCalledTimes(1);
});

// A write of the description that ends without landing - refused, as here, or stopped - leaves
// the reader's text in the editor, so nothing they wrote is lost, and the editor does not write
// it again on its own: the shell's pending-writes line holds the refused write, with its Retry,
// and a conflict's text is offered beside the editor (`ChangedMeanwhile`). So the next leave
// writes nothing.
test("a refused description stays the reader's text in the editor, and the next leave leaves it to the shell's Retry", async () => {
  const { answering, editor, onSave } = described("Speedruns run again.");
  const answer = answering();
  const { box, handle } = await editor();
  handle.view.focus();
  writeAtEnd(handle, " More.");
  act(() => handle.view.dom.blur());
  expect(onSave.mock.calls).toEqual([["Speedruns run again. More.\n", "Speedruns run again."]]);
  await answer(0, false);
  expect(box.textContent).toBe("Speedruns run again. More.");
  handle.view.focus();
  act(() => handle.view.dom.blur());
  expect(onSave).toHaveBeenCalledTimes(1);
});

// The read after the first of two writes lands while the reader still holds the editor: the
// second text is what Dispatch is about to hold, so leaving writes nothing more.
test("the answer to an earlier description write landing under the reader writes the later text once", async () => {
  const { editor, onSave, rerender } = described("");
  const { box, handle } = await editor();
  handle.view.focus();
  await onFakeClock(async () => {
    writeAtEnd(handle, "A");
    await jump(SAVE_AFTER_MS);
    writeAtEnd(handle, "B");
    await jump(SAVE_AFTER_MS);
  });
  expect(onSave.mock.calls).toEqual([
    ["A\n", ""],
    ["AB\n", ""],
  ]);
  rerender("A\n");
  expect(box.textContent).toBe("AB");
  act(() => handle.view.dom.blur());
  expect(onSave).toHaveBeenCalledTimes(2);
});

// A description is a stored text its writer typed in a box with the reference picker, and the
// priorities read sends the reading the server made of it (`description_tree`): a reader who may
// not write reads that tree in the editor, and nothing on the page's thread parses the text. The
// tree reads differently from the markdown beside it, so an editor filled from a parse of the
// markdown would end `daemon.`.
test("a description is drawn from the tree the priorities read sends", async () => {
  const tree = displayTree("Ship the **daemon** first.");
  const parses = countEngineParses();
  try {
    const { editor, imports } = described("Ship the **daemon**.", false, tree);
    const { box } = await editor();
    expect(box.textContent).toBe("Ship the daemon first.");
    expect(box.querySelector("strong")?.textContent).toBe("daemon");
    expect(imports).toEqual([]);
    expect(parses.count()).toBe(0);
  } finally {
    parses.restore();
  }
});

// A writer's editor hosts the reference picker every box has, under the writing list's row of a
// priority's description, whose words stand under it; a reader who may not write it has nothing
// to pick, and reads no such line.
test("a writer's editor hosts the reference picker under the description's row of the writing list, and a reader's does not", async () => {
  const hint = dispatchWritingBox("priority-description", "priority-description").hint;
  const writer = described("Speedruns run again.");
  await writer.editor();
  expect(writer.scope.getByTestId("priority-description-hint").textContent).toBe(hint);
  writer.view.unmount();

  const reader = described("Speedruns run again.", false);
  await reader.editor();
  expect(reader.scope.queryByTestId("priority-description-hint")).toBeNull();
});
