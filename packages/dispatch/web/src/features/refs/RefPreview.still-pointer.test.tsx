import { afterEach, expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: this row needs a pointer event user-event cannot send (a pointerType and a relatedTarget).
import { act, fireEvent, render, screen } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import { RefPreviewHost } from "./RefPreview";
import { closeRefPreview, referenceTriggerProps } from "./ref-preview-state";
import { REF_PREVIEW_CLOSE_DELAY_MS, REF_PREVIEW_OPEN_DELAY_MS } from "./ref-preview-timing";

// A card the reader asked for is not taken away because the page moved, and that holds for a
// hover still waiting out its delay as much as for a card already drawn: the reader pointed at
// the reference and held still, and nothing else is coming to open it, because the pointer never
// moved. The browser row is `e2e/heavy-bodies.e2e.ts`'s hover card of a comment of 8,000 list
// items, whose page draws its header, its tab strip and its margin over the body while the delay
// runs.
//
// A file of its own, because the host reads a boundary event within `READER_INPUT_GRACE_MS` of
// the reader's own last pointer movement as the reader's: that reading is module state shared by
// every row of a file, and happy-dom's event timestamps are the wall clock, which no fake timer
// of a row moves. Here nothing has moved a pointer, so the page's move is the page's.

afterEach(() => {
  jest.useRealTimers();
  closeRefPreview();
});

function Harness(): ReactNode {
  return (
    <MemoryRouter>
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <a href="/issues/CORE-1" {...referenceTriggerProps({ key: "CORE-1", kind: "issue" })}>
          the reference
        </a>
        <RefPreviewHost />
      </QueryClientProvider>
    </MemoryRouter>
  );
}

test("a reference that slid out from under a still pointer opens its card, and the reader's next movement decides it", () => {
  // The card's own read is never answered: what this row is about is the card opening, and until
  // its target answers the card heads with the reference's short form.
  const getIssue = spyOn(api, "getIssue").mockImplementation(() => new Promise(() => {}));
  jest.useFakeTimers();
  const view = render(<Harness />);
  try {
    const reference = screen.getByRole("link", { name: "the reference" });
    // A hover-intent pointer: user-event's hover names no pointerType, which this site needs.
    fireEvent.pointerOver(reference, { pointerType: "mouse" });

    // The page draws something above the reference while the open delay runs, so the anchor
    // moves and the browser reports the pointer leaving it with no movement of the reader's
    // behind it. happy-dom lays nothing out, so the move is the measurement moving.
    spyOn(reference, "getBoundingClientRect").mockReturnValue({
      left: 0,
      top: 24,
    } as DOMRect);
    fireEvent.pointerOut(reference, { pointerType: "mouse", relatedTarget: document.body });

    // The request is kept, not granted: the reader's next movement is what decides it, and this
    // one lands on the reference where the page left it, so the card they asked for opens on
    // the delay it was already waiting out.
    fireEvent.pointerMove(reference, { clientX: 10, clientY: 30, pointerType: "mouse" });
    act(() => {
      jest.advanceTimersByTime(REF_PREVIEW_OPEN_DELAY_MS);
    });
    expect(screen.getByRole("tooltip").textContent).toContain("CORE-1");

    // And a movement that leaves the reference closes it as any other does, so a card kept
    // under a still pointer never stands over nothing once that pointer has moved. The anchor
    // stays where the page left it this time, so nothing but the reader has moved.
    fireEvent.pointerOut(reference, { pointerType: "mouse", relatedTarget: document.body });
    act(() => {
      jest.advanceTimersByTime(REF_PREVIEW_CLOSE_DELAY_MS);
    });
    expect(screen.queryByRole("tooltip")).toBeNull();
  } finally {
    view.unmount();
    getIssue.mockRestore();
  }
});

test("a reference the page moves in the moment the pointer reaches it still opens its card", () => {
  // The row above has nothing move the pointer at all, which no reader can do: a reader reaches
  // a reference by moving onto it, and that movement lands milliseconds before the page draws
  // whatever slides the reference away. So this row sends the arrival first, and the page's move
  // inside the same instant - which is `e2e/hover-preview.e2e.ts`'s ordinary conversation page in
  // CI's container (run 42628596511, shard 2, red on both attempts: the card never opened).
  // Nothing about the arrival says the reader is leaving; the anchor's measured box is what says
  // the page moved, and their next movement is what decides a request kept on it.
  const getIssue = spyOn(api, "getIssue").mockImplementation(() => new Promise(() => {}));
  jest.useFakeTimers();
  const view = render(<Harness />);
  try {
    const reference = screen.getByRole("link", { name: "the reference" });
    fireEvent.pointerMove(reference, { clientX: 10, clientY: 30, pointerType: "mouse" });
    fireEvent.pointerOver(reference, { pointerType: "mouse" });

    spyOn(reference, "getBoundingClientRect").mockReturnValue({
      left: 0,
      top: 24,
    } as DOMRect);
    fireEvent.pointerOut(reference, { pointerType: "mouse", relatedTarget: document.body });

    act(() => {
      jest.advanceTimersByTime(REF_PREVIEW_OPEN_DELAY_MS);
    });
    expect(screen.getByRole("tooltip").textContent).toContain("CORE-1");
  } finally {
    view.unmount();
    getIssue.mockRestore();
  }
});
