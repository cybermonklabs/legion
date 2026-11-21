import { expect, test } from "bun:test";

import { holdSection } from "./pane-placement";

/** Short enough that no row waits for the real numbers, long enough that a slow machine is not
 *  read as a settled drawing. */
const QUIET_MS = 150;
const BOUNDS = { limitMs: 2_000, quietMs: QUIET_MS };
/** Past the quiet, whatever the machine was doing in between. */
const AFTER_QUIET_MS = QUIET_MS * 3;

/** A column with something drawn in it, which is where a reader's press lands. */
function column(): { box: HTMLElement; inside: HTMLElement } {
  const box = document.createElement("div");
  const inside = document.createElement("p");
  box.append(inside);
  document.body.append(box);
  return { box, inside };
}

test("a column nobody has touched is written while it draws, and is let go at the bound", async () => {
  const { box } = column();
  const hold = holdSection(box, BOUNDS);
  try {
    // The document drawing itself is not the reader moving: the placement is applied again for
    // every block that lands above the heading, which is the defect this hold exists for.
    hold.drawing();
    expect(hold.writes()).toBe(true);
    await Bun.sleep(AFTER_QUIET_MS);
    expect(hold.writes()).toBe(false);
  } finally {
    hold.release();
    box.remove();
  }
});

test("a hold nothing has drawn in writes on, however long it waits", async () => {
  const { box } = column();
  const hold = holdSection(box, { limitMs: QUIET_MS, quietMs: QUIET_MS });
  try {
    // No drawing pass has started the clocks, so neither can end the hold: the pane writes the
    // column only from the pass that tells the hold, and its own cleanup releases it.
    await Bun.sleep(AFTER_QUIET_MS);
    expect(hold.writes()).toBe(true);
  } finally {
    hold.release();
    box.remove();
  }
});

test("a hold is let go once the drawing has been quiet", async () => {
  const { box } = column();
  const hold = holdSection(box, BOUNDS);
  try {
    hold.drawing();
    hold.drawing();
    expect(hold.writes()).toBe(true);
    await Bun.sleep(AFTER_QUIET_MS);
    expect(hold.writes()).toBe(false);
  } finally {
    hold.release();
    box.remove();
  }
});

test("a drawing pass inside the quiet keeps the hold", async () => {
  const { box } = column();
  const hold = holdSection(box, BOUNDS);
  try {
    hold.drawing();
    await Bun.sleep(5);
    hold.drawing();
    await Bun.sleep(5);
    expect(hold.writes()).toBe(true);
  } finally {
    hold.release();
    box.remove();
  }
});

test("a document that keeps drawing is let go at the limit", async () => {
  const { box } = column();
  // A quiet longer than this row takes: the limit is the only clock that can end this hold.
  const hold = holdSection(box, { limitMs: 100, quietMs: 2_000 });
  try {
    for (let pass = 0; pass < 10; pass += 1) {
      hold.drawing();
      await Bun.sleep(20);
    }
    expect(hold.writes()).toBe(false);
  } finally {
    hold.release();
    box.remove();
  }
});

test("a reader action inside the quiet lets the column go at once", async () => {
  const { box, inside } = column();
  const hold = holdSection(box, BOUNDS);
  try {
    hold.drawing();
    await Bun.sleep(5);
    inside.dispatchEvent(new Event("pointerdown", { bubbles: true }));
    expect(hold.writes()).toBe(false);
  } finally {
    hold.release();
    box.remove();
  }
});

for (const action of ["pointerdown", "wheel", "touchstart", "keydown"]) {
  test(`${action} in the column is the reader moving on, so the column is let go`, () => {
    const { box, inside } = column();
    const hold = holdSection(box);
    try {
      inside.dispatchEvent(new Event(action, { bubbles: true }));
      expect(hold.writes()).toBe(false);
    } finally {
      hold.release();
      box.remove();
    }
  });
}

// A modifier pressed alone neither moves the reader nor writes anything, so the column is still
// the placement's. On the real page a reader who stepped into the column with `l` and pressed
// Shift before its document was built was left at the document's start, the section they had
// opened 3,406.6 px under the top of the column.
test("a modifier pressed alone in the column leaves it held", () => {
  const { box, inside } = column();
  const hold = holdSection(box);
  try {
    for (const key of [
      "Alt",
      "AltGraph",
      "CapsLock",
      "Control",
      "Meta",
      "NumLock",
      "ScrollLock",
      "Shift",
    ]) {
      inside.dispatchEvent(new KeyboardEvent("keydown", { bubbles: true, key }));
      expect({ key, writes: hold.writes() }).toEqual({ key, writes: true });
    }
  } finally {
    hold.release();
    box.remove();
  }
});

// Every other key is the reader working in the column, a letter typed into a field of it
// included. That is this hold's own rule and not `movesTheReader`'s (`readers-scroll.ts`), under
// which a typed letter moves nobody: with the note field opened by Enter and a letter typed
// into it, the field stood 505.7 px down the column and stayed, where a hold still standing put
// the section's heading back at the top and the field 191 px further down.
test("a letter typed into a field of the column lets it go, and so does the key that completes a copy", () => {
  for (const [name, init] of [
    ["a letter", { key: "L", shiftKey: true }],
    ["a copy", { ctrlKey: true, key: "c" }],
    ["Space", { key: " " }],
    ["an arrow", { key: "ArrowDown" }],
  ] as const) {
    const { box } = column();
    const note = document.createElement("textarea");
    box.append(note);
    const hold = holdSection(box);
    try {
      note.dispatchEvent(new KeyboardEvent("keydown", { ...init, bubbles: true }));
      expect({ name, writes: hold.writes() }).toEqual({ name, writes: false });
    } finally {
      hold.release();
      box.remove();
    }
  }
});

// Selecting another row is a press in the list beside the column, and the column it opens is
// placed at its section like any other.
test("a press outside the column leaves it held", () => {
  const { box } = column();
  const elsewhere = document.createElement("button");
  document.body.append(elsewhere);
  const hold = holdSection(box);
  try {
    elsewhere.dispatchEvent(new Event("pointerdown", { bubbles: true }));
    expect(hold.writes()).toBe(true);
  } finally {
    hold.release();
    box.remove();
    elsewhere.remove();
  }
});

test("an event of another kind leaves the column written until the bound", async () => {
  const { box, inside } = column();
  const hold = holdSection(box, BOUNDS);
  try {
    hold.drawing();
    inside.dispatchEvent(new Event("scroll", { bubbles: true }));
    inside.dispatchEvent(new Event("pointermove", { bubbles: true }));
    expect(hold.writes()).toBe(true);
    await Bun.sleep(AFTER_QUIET_MS);
    expect(hold.writes()).toBe(false);
  } finally {
    hold.release();
    box.remove();
  }
});

test("a released hold stays let go, whatever the reader does next", () => {
  const { box, inside } = column();
  const hold = holdSection(box);
  hold.release();

  expect(hold.writes()).toBe(false);
  inside.dispatchEvent(new Event("pointerdown", { bubbles: true }));
  expect(hold.writes()).toBe(false);
  box.remove();
});
