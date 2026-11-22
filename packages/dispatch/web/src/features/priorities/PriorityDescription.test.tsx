import { afterEach, beforeEach, expect, test } from "bun:test";
import { act, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { displayTree } from "../../__tests__/display-trees";
import { renderWithQueries as render } from "../../__tests__/query-provider";
import type { DisplayTree } from "../../api/types";
import { loadedEngine, warmMarkdownRenderer } from "../refs/markdown-engine";
import {
  flushTasks,
  installFakeParseWorker,
  restoreParseWorker,
} from "../refs/parse-worker-fixture";
import { PriorityDescription } from "./PriorityDescription";

/** Emphasis nested 4,000 deep, 24,001 characters: the shape micromark spends 672,095,992
 *  `attention` steps on, about 55 s (cybermonklabs/legion#4437). */
const NESTED = `${"*a ".repeat(4_000)}b${" c*".repeat(4_000)}`;

/** The server's reading of `NESTED`, as Go's display read writes it for marks nested past its
 *  bound (pmdoc's `maxInlineNesting`, `literalDocument`): one paragraph of the text's own
 *  characters, no mark among them. It is written out here because the page's own engine, which
 *  `displayTree` reads with, is the parser this test keeps the page from running. */
const NESTED_TREE: DisplayTree = {
  content: [{ content: [{ text: NESTED, type: "text" }], type: "paragraph" }],
  type: "doc",
};

beforeEach(async () => {
  await warmMarkdownRenderer();
});

afterEach(restoreParseWorker);

/** Counts every call of the shared read-only engine's `parse`, the one entry a display surface
 *  parses markdown on the page through (`e2e/parse-counting.ts`), until the returned restore:
 *  the length of each text parsed, so a failure names how much and prints none of it. */
function countPageParses(): { parsed: number[]; restore: () => void } {
  const engine = loadedEngine();
  if (engine === undefined) throw new Error("the row needs the shared engine loaded");
  const counted = engine as { parse: typeof engine.parse };
  const parse = counted.parse;
  const parsed: number[] = [];
  counted.parse = (markdown, softBreaks) => {
    parsed.push(markdown.length);
    return parse.call(engine, markdown, softBreaks);
  };
  return {
    parsed,
    restore: () => {
      counted.parse = parse;
    },
  };
}

test("a stored description is drawn from the server's tree, and nothing on the page or in the parse worker parses it", async () => {
  const worker = installFakeParseWorker();
  const { parsed, restore } = countPageParses();
  const view = render(
    <PriorityDescription
      canWrite={false}
      description={NESTED}
      descriptionTree={NESTED_TREE}
      label="Description of P1"
      onSave={() => {}}
    />
  );
  try {
    const body = view.container.firstElementChild as HTMLElement;
    // Drawn in the commit that mounted it, with no parse owed: a text with no tree this long
    // would be the worker's, marked pending meanwhile.
    expect(body.getAttribute("data-markdown-pending")).toBeNull();
    expect(body.textContent).toBe(NESTED);
    expect(body.querySelector("em, strong")).toBeNull();
    await act(() => flushTasks());
    expect(worker.requests.map((request) => request.markdown.length)).toEqual([]);
    expect(parsed).toEqual([]);
  } finally {
    restore();
    view.unmount();
  }
});

test("a description the reader has just written is drawn from its own text, never from the tree of the stored one", async () => {
  const user = userEvent.setup();
  const stored = "The **old** plan.";
  render(
    <PriorityDescription
      canWrite
      description={stored}
      descriptionTree={displayTree(stored)}
      label="Description of P1"
      onSave={() => {}}
    />
  );
  const described = screen.getByRole("group", { name: "Description of P1" });
  expect(within(described).getByText("old").tagName).toBe("STRONG");
  await user.click(within(described).getByRole("button", { name: "Edit the description of P1" }));
  const field = await screen.findByRole("textbox", { name: "Description of P1" });
  await user.clear(field);
  await user.type(field, "The *new* plan.");
  await user.tab();
  // The server still answers the old text and its tree, and the block shows what was written.
  const back = await screen.findByRole("group", { name: "Description of P1" });
  expect(within(back).getByText("new").tagName).toBe("EM");
  expect(within(back).queryByText("old")).toBeNull();
});
