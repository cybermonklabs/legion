import { expect, test } from "bun:test";
import { TextSelection } from "@milkdown/kit/prose/state";
import type { EditorView } from "@milkdown/kit/prose/view";
import type { MarkAction } from "../src/lib";
import { withMarksEditor } from "./marks-editor";

const SENTENCE = "The quick brown fox";

function select(view: EditorView, from: number, to: number): void {
  view.dispatch(view.state.tr.setSelection(TextSelection.create(view.state.doc, from, to)));
}

/** The selection bar's buttons, by their labels. */
function barLabels(view: EditorView): string[] {
  const bar = view.dom.ownerDocument.querySelector(".dispatch-action-bar");
  return Array.from(bar?.querySelectorAll("button") ?? [], (button) =>
    button.hidden ? "" : (button.textContent ?? "")
  ).filter((label) => label !== "");
}

test("defineSelection calls onDefine for a selection that holds text, on every access, and marks nothing", async () => {
  let defined = 0;
  const actions: MarkAction[] = [];
  await withMarksEditor(
    SENTENCE,
    ({ handle, view }) => {
      select(view, 5, 16);
      expect(handle.defineSelection()).toBe(true);
      expect(defined).toBe(1);

      // A read-only document still defines its words, and Define writes no mark.
      handle.setAccess("read");
      select(view, 1, 4);
      expect(handle.defineSelection()).toBe(true);
      expect(defined).toBe(2);
      expect(actions).toEqual([]);

      // An empty selection defines nothing.
      select(view, 5, 5);
      expect(handle.defineSelection()).toBe(false);
      expect(defined).toBe(2);
    },
    {
      onDefine: () => {
        defined += 1;
      },
      onMarkAction: (action) => void actions.push(action),
    }
  );
});

test("defineSelection does nothing in an editor given no onDefine, whose bar offers no Define", async () => {
  await withMarksEditor(SENTENCE, ({ handle, view }) => {
    select(view, 5, 16);
    expect(handle.defineSelection()).toBe(false);
    expect(barLabels(view)).not.toContain("Define");
  });
});

test("the selection bar offers Define beside Comment, Suggest and Ask where the host offers it", async () => {
  await withMarksEditor(
    SENTENCE,
    ({ view }) => {
      const bar = view.dom.ownerDocument.querySelector(".dispatch-action-bar");
      const labels = Array.from(
        bar?.querySelectorAll("button") ?? [],
        (button) => button.textContent
      );
      expect(labels).toEqual(["Comment", "Suggest", "Ask", "Define"]);
    },
    { onDefine: () => {} }
  );
});
