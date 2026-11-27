import { expect, test } from "bun:test";
import { act } from "@testing-library/react";

import { openEarlyCaret } from "./early-caret";
import { KEY_REPORTS } from "./page-structure";

const TEXT = "First line.";

function setup(text = TEXT, offset = 0, editable = true) {
  const row = document.body.appendChild(document.createElement("div"));
  row.tabIndex = 0;
  const words = row.appendChild(document.createElement("p"));
  words.textContent = text;
  const reports: string[] = [];
  const starts: string[] = [];
  const replay: string[] = [];
  let boundText = text;
  let boundCaret = -1;
  let ended = 0;
  const hold = openEarlyCaret({
    row,
    words,
    offset,
    editable,
    start: () => starts.push("work"),
    report: (line: string) => reports.push(line),
  });
  const input = () =>
    row.querySelector<HTMLElement>('[data-editor-box] [contenteditable="plaintext-only"]');
  const editor = {
    replace(from: number, to: number, insert: string) {
      replay.push(`replace ${from}:${to} ${insert}`);
      boundText = boundText.slice(0, from) + insert + boundText.slice(to);
    },
    keydown(event: KeyboardEvent) {
      replay.push(event.key);
    },
    select(at: number) {
      boundCaret = at;
    },
    end() {
      ended++;
    },
    positionAt: (at: number): number | undefined => at,
  };
  const event = (key: string, init: KeyboardEventInit = {}) => {
    const target = input();
    if (target === null) throw new Error("no stand-in to receive a key");
    return target.dispatchEvent(
      new KeyboardEvent("keydown", { key, bubbles: true, cancelable: true, ...init })
    );
  };
  const write = (value: string, caret = value.length, inputType = "insertText") => {
    const target = input();
    if (!target) throw new Error("no stand-in to type into");
    target.dispatchEvent(new InputEvent("beforeinput", { bubbles: true, inputType }));
    target.textContent = value;
    if (!target.firstChild) target.appendChild(document.createTextNode(""));
    document.getSelection()?.collapse(target.firstChild, caret);
    target.dispatchEvent(new InputEvent("input", { bubbles: true, inputType }));
  };
  const composition = (
    name: "compositionstart" | "compositionupdate" | "compositionend",
    data: string
  ) => {
    input()?.dispatchEvent(new CompositionEvent(name, { bubbles: true, data }));
  };
  return {
    row,
    hold,
    input,
    editor,
    starts,
    reports,
    replay,
    event,
    write,
    composition,
    text: () => boundText,
    caret: () => boundCaret,
    ended: () => ended,
    selection: () => document.getSelection()?.anchorOffset,
    cleanup() {
      hold?.destroy();
      row.remove();
    },
  };
}

function early(
  name: string,
  run: (rig: ReturnType<typeof setup>) => void | Promise<void>,
  offset = 0,
  editable = true
) {
  test(name, async () => {
    const f = setup(TEXT, offset, editable);
    try {
      await act(() => run(f));
    } finally {
      f.cleanup();
    }
  });
}

early("Enter draws the focused stand-in at offset zero before the room is ready", (f) => {
  expect(f.input()).toBe<Element | null>(document.activeElement);
  expect(f.input()?.textContent).toBe(TEXT);
  expect(document.getSelection()?.isCollapsed).toBe(true);
  expect(f.selection()).toBe(0);
  expect(f.starts).toEqual(["work"]);
});

early(
  "a click draws the caret at the pressed offset before the bind",
  (f) => {
    expect(f.input()).toBe<Element | null>(document.activeElement);
    expect(f.selection()).toBe(4);
  },
  4
);

early("characters including j and k draw in the stand-in without a write to the room", (f) => {
  const outer = (event: Event) => {
    if (event.target === f.row) throw new Error("a typed key reached the row");
  };
  f.row.addEventListener("keydown", outer);
  f.write(`jk${TEXT}`, 2);
  expect(f.input()?.textContent).toBe(`jk${TEXT}`);
  expect(f.text()).toBe(TEXT);
  expect(f.replay).toEqual([]);
});

early("a composition's intermediate text draws without reaching the room", (f) => {
  f.composition("compositionstart", "");
  f.composition("compositionupdate", "に");
  f.write(`に${TEXT}`, 1, "insertCompositionText");
  expect(f.input()?.textContent).toBe(`に${TEXT}`);
  expect(f.replay).toEqual([]);
});

early("a ready editor waits for compositionend before applying the text", async (f) => {
  f.composition("compositionstart", "");
  f.write(`に${TEXT}`, 1, "insertCompositionText");
  const bind = f.hold?.bind(f.editor);
  expect(f.replay).toEqual([]);
  f.write(`日本${TEXT}`, 2, "insertCompositionText");
  f.composition("compositionend", "日本");
  await bind;
  expect(f.text()).toBe(`日本${TEXT}`);
  expect(f.caret()).toBe(2);
});

const heldKeys: readonly [string, string, KeyboardEventInit][] = [
  ["return", "Enter", {}],
  ["tab", "Tab", {}],
  ["undo", "z", { ctrlKey: true }],
  ["redo", "z", { ctrlKey: true, shiftKey: true }],
  ["comment chord", "m", { ctrlKey: true, altKey: true }],
  ["Mod-Alt-m", "m", { metaKey: true, altKey: true }],
  ["backspace at the start", "Backspace", {}],
];

early("a held key snapshots the text without changing the stand-in before the bind", async (f) => {
  for (const [, key, init] of heldKeys) expect(f.event(key, init)).toBe(false);
  expect(f.input()?.textContent).toBe(TEXT);
  expect(f.replay).toEqual([]);
  await f.hold?.bind(f.editor);
  expect(f.replay).toEqual(heldKeys.map(([, key]) => key));
});

early("every kind of key typed before the bind is applied at the bind, in order", async (f) => {
  f.write(`abc${TEXT}`, 3);
  f.composition("compositionstart", "");
  f.composition("compositionupdate", "に");
  f.write(`abcに${TEXT}`, 4, "insertCompositionText");
  f.write(`abc日本${TEXT}`, 5, "insertCompositionText");
  f.composition("compositionend", "日本");
  f.write(`abc日本dez${TEXT}`, 9);
  expect(f.event("Backspace")).toBe(true);
  f.write(`abc日本de${TEXT}`, 8, "deleteContentBackward");
  expect(f.event("Delete")).toBe(true);
  f.write(`abc日本de${TEXT.slice(1)}`, 8, "deleteContentForward");
  expect(f.event("ArrowLeft")).toBe(true);
  expect(f.input()?.firstChild).not.toBeNull();
  document.getSelection()?.collapse(f.input()?.firstChild ?? null, 0);
  for (const [kind, key, init] of heldKeys) expect(f.event(key, init), kind).toBe(false);
  f.event("Escape");
  await f.hold?.bind(f.editor);
  expect(f.text()).toBe(`abc日本de${TEXT.slice(1)}`);
  expect(f.replay[0]).toMatch(/^replace /);
  expect(f.replay.slice(1)).toEqual(heldKeys.map(([, key]) => key));
  expect(f.caret()).toBe(0);
  expect(f.ended()).toBe(1);
});

early("replay keeps the composed text in order and the caret after e", async (f) => {
  f.write(`abc${TEXT}`, 3);
  f.composition("compositionstart", "");
  f.write(`abc日本${TEXT}`, 5, "insertCompositionText");
  f.composition("compositionend", "日本");
  f.write(`abc日本de${TEXT}`, 7);
  await f.hold?.bind(f.editor);
  expect(f.text()).toBe(`abc日本de${TEXT}`);
  expect(f.caret()).toBe(7);
  expect(f.input()).toBeNull();
});

early(
  "Backspace and Delete inside the caret's text are applied in the final snapshot",
  async (f) => {
    f.write(`XYZ${TEXT}`, 2);
    expect(f.event("Backspace")).toBe(true);
    f.write(`XZ${TEXT}`, 1, "deleteContentBackward");
    expect(f.event("Delete")).toBe(true);
    f.write(`X${TEXT}`, 1, "deleteContentForward");
    await f.hold?.bind(f.editor);
    expect(f.text()).toBe(`X${TEXT}`);
    expect(f.caret()).toBe(1);
  }
);

early("Escape restores focus now and still applies typed text at the bind", async (f) => {
  f.write(`x${TEXT}`, 1);
  f.event("Escape");
  expect(f.input()).toBeNull();
  expect(f.row).toBe<Element | null>(document.activeElement);
  await f.hold?.bind(f.editor);
  expect(f.text()).toBe(`x${TEXT}`);
  expect(f.ended()).toBe(1);
});

early("a refused bind restores the row and reports unsaved text", (f) => {
  f.write(`lost${TEXT}`, 4);
  f.hold?.refuse();
  expect(f.input()).toBeNull();
  expect(f.row.textContent).toBe(TEXT);
  expect(f.reports).toEqual([KEY_REPORTS.editor_text_not_saved]);
});

early(
  "a second row can take the caret while the first keeps its queued text for its own bind",
  async (a) => {
    a.write(`A${TEXT}`, 1);
    a.hold?.leave();
    const b = setup();
    try {
      b.write(`B${TEXT}`, 1);
      expect(a.input()).toBeNull();
      expect(b.input()).toBe<Element | null>(document.activeElement);
      await a.hold?.bind(a.editor);
      await b.hold?.bind(b.editor);
      expect([a.text(), b.text()]).toEqual([`A${TEXT}`, `B${TEXT}`]);
    } finally {
      b.cleanup();
    }
  }
);

early(
  "unmapped drawn offsets fall back to the nearest earlier position without dropping the text",
  async (f) => {
    f.write(`${TEXT.slice(0, 4)}x${TEXT.slice(4)}`, 5);
    await f.hold?.bind({
      ...f.editor,
      positionAt: (at: number) => (at > 2 && at < 6 ? undefined : at),
    });
    expect(f.text()).toBe(`${TEXT.slice(0, 2)}x${TEXT.slice(2)}`);
  }
);

early(
  "a row that cannot take typing starts the work without a stand-in or a refusal",
  (f) => {
    expect(f.input()).toBeNull();
    expect(f.starts).toEqual(["work"]);
    expect(f.reports).toEqual([]);
  },
  0,
  false
);

for (const typed of [false, true]) {
  early(
    `read-only handover discards the stand-in ${typed ? "after typing" : "without typing"}`,
    (f) => {
      if (typed) f.write(`lost${TEXT}`, 4);
      let reused: HTMLElement | undefined;
      f.hold?.handover((box) => {
        reused = box;
        box?.append(document.createTextNode(TEXT));
        f.hold?.refuse();
      });
      expect(f.input()).toBeNull();
      expect(reused?.textContent).toBe(TEXT);
      expect(f.replay).toEqual([]);
      expect(f.reports).toEqual(typed ? [KEY_REPORTS.editor_text_not_saved] : []);
    }
  );
}
