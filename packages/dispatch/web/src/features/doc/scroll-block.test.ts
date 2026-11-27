import { expect, test } from "bun:test";
import { waitFor } from "@testing-library/react";

import { installResizeObserver } from "../../__tests__/scroll-box";
import { cancelBlockScroll, scrollBlockToTop } from "./scroll-block";

const CONTAINER_TOP = 100;

/** A scrolling container whose top edge is at `CONTAINER_TOP`, holding one block whose top edge
 * is `below` pixels under it. happy-dom lays nothing out, so both boxes are the ones stated. */
function containerHolding(blockId: string, below: number) {
  const container = document.createElement("div");
  container.getBoundingClientRect = () => ({ top: CONTAINER_TOP }) as DOMRect;
  const block = document.createElement("section");
  block.setAttribute("data-block-id", blockId);
  block.getBoundingClientRect = () => ({ top: CONTAINER_TOP + below }) as DOMRect;
  container.append(block);
  return container;
}

test("the block the reader chose is scrolled to the top of its container", () => {
  const container = containerHolding("b-1", 240);
  container.scrollTop = 60;

  expect(scrollBlockToTop("b-1", container)).toBe(true);
  expect(container.scrollTop).toBe(300);
});

test("a block the container does not hold answers false and scrolls nothing", () => {
  const container = containerHolding("b-1", 240);
  container.scrollTop = 60;

  expect(scrollBlockToTop("b-2", container)).toBe(false);
  expect(container.scrollTop).toBe(60);
});

test("a container already showing the block at its top is not moved", () => {
  const container = containerHolding("b-1", 0);
  container.scrollTop = 420;

  expect(scrollBlockToTop("b-1", container)).toBe(true);
  expect(container.scrollTop).toBe(420);
});

test("a block id a CSS selector would read as syntax is found by its own name", () => {
  const container = containerHolding("ship it.now", 120);
  container.scrollTop = 0;

  expect(scrollBlockToTop("ship it.now", container)).toBe(true);
  expect(container.scrollTop).toBe(120);
});

// The skip's re-run watches the document drawn in the column. The blocks between the reader's
// place and the block they skipped to come into view with the skip, and each one the browser
// skipped (`content-visibility: auto`) paints at its own height in place of its estimate, which
// pushes the block down: with no drawn root to watch and no diagram to wait on there was no
// re-run, and a phone's reading view was left a screen above the answer (CI run 45994800355,
// `inbox-reading-pane.e2e.ts`). happy-dom lays nothing out, so the column's overflow and sizes,
// and where the document and the block stand in it, are stated here.
test("a block the reader skipped to is put back at the top when the document drawn above it grows", async () => {
  const sizes = installResizeObserver();
  let grown = 0;
  const container = document.createElement("div");
  container.style.overflowY = "auto";
  Object.defineProperty(container, "scrollHeight", { configurable: true, get: () => 4_000 });
  Object.defineProperty(container, "clientHeight", { configurable: true, get: () => 400 });
  container.getBoundingClientRect = () => ({ height: 400, top: CONTAINER_TOP }) as DOMRect;
  const drawn = document.createElement("div");
  drawn.getBoundingClientRect = () =>
    ({ height: 1_000 + grown, top: CONTAINER_TOP - container.scrollTop }) as DOMRect;
  const block = document.createElement("section");
  block.setAttribute("data-block-id", "answer");
  block.getBoundingClientRect = () =>
    ({ top: CONTAINER_TOP + 600 + grown - container.scrollTop }) as DOMRect;
  drawn.append(block);
  container.append(drawn);
  document.body.append(container);
  try {
    expect(scrollBlockToTop("answer", container, drawn)).toBe(true);
    expect(container.scrollTop).toBe(600);

    // The blocks above it paint at their own height: 450 px more than their estimates.
    grown = 450;
    sizes.resize(drawn);
    await waitFor(() => expect(container.scrollTop).toBe(1_050));
  } finally {
    cancelBlockScroll();
    container.remove();
    sizes.restore();
  }
});
