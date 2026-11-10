import { afterEach, expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: these rows need pointer events user-event cannot send (a pointerType, a relatedTarget, and a place of the pointer's own).
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
// What tells the page's doing from the reader's is the place the boundary event carries: the
// browser dispatches the pair at the pointer's own `clientX`/`clientY` when the page moves the
// zone out from under it, and at the new place - the one the `pointermove` right behind it
// carries - when the reader moves away. So every row here sends the arrival's movement and then
// the boundary event at the place that movement left the pointer, which is what the browser
// sends.
//
// A request so kept is not a card so drawn: it opens only where the reference is what the
// pointer is on when its delay runs out, and waits for the reader's next movement where it is
// not (`RefPreview.tsx`, `thePointerIsOn`). That question is about the laid-out page, which
// happy-dom does not lay out, so each row below says what the page would answer.
//
// A file of its own, because the pointer this host has last seen move is state every row of a
// file shares, as is the reading of a scroll within `READER_INPUT_GRACE_MS` of the reader's last
// input. Here nothing has moved a pointer but these rows.

afterEach(() => {
  jest.useRealTimers();
  closeRefPreview();
});

function Page({ host = true }: { host?: boolean }): ReactNode {
  return (
    <MemoryRouter>
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <a href="/issues/CORE-1" {...referenceTriggerProps({ key: "CORE-1", kind: "issue" })}>
          the reference
        </a>
        {host ? <RefPreviewHost /> : null}
      </QueryClientProvider>
    </MemoryRouter>
  );
}

/** What the laid-out page would answer about `reference`: the box it stands in, and what the
 *  reader's pointer is on there - the reference itself unless something of the page's is drawn
 *  over it. Returns the way to put both readings back. */
function thePageDraws(
  reference: HTMLElement,
  box: { bottom: number; left: number; right: number; top: number },
  onTop: Element = reference
): () => void {
  const measured = spyOn(reference, "getBoundingClientRect").mockReturnValue(box as DOMRect);
  const atThePoint = spyOn(document, "elementFromPoint").mockReturnValue(onTop);
  return () => {
    measured.mockRestore();
    atThePoint.mockRestore();
  };
}

test("a reference that slid out from under a still pointer opens its card, and the reader's next movement decides it", () => {
  // The card's own read is never answered: what this row is about is the card opening, and until
  // its target answers the card heads with the reference's short form.
  const getIssue = spyOn(api, "getIssue").mockImplementation(() => new Promise(() => {}));
  jest.useFakeTimers();
  const view = render(<Page />);
  // Where the page leaves the reference: over both the place the reader's pointer rests and the
  // place they move it to below, and with nothing drawn over it.
  const restoreThePage = thePageDraws(screen.getByRole("link", { name: "the reference" }), {
    bottom: 40,
    left: 0,
    right: 100,
    top: 20,
  });
  try {
    const reference = screen.getByRole("link", { name: "the reference" });
    // The reader reaching the reference: a movement onto it, then the hover itself. user-event's
    // hover sends neither the place the host tells a moved pointer by nor the `pointerType` its
    // hover-intent gate reads.
    fireEvent.pointerMove(reference, { clientX: 10, clientY: 30, pointerType: "mouse" });
    fireEvent.pointerOver(reference, { pointerType: "mouse" });

    // The page draws something above the reference while the open delay runs, so the reference
    // leaves the pointer. The pointer is still at 10, 30 - where the reader left it - and the
    // page has nothing else to send: no movement of theirs is coming to open a second hover.
    fireEvent.pointerOut(reference, {
      clientX: 10,
      clientY: 30,
      pointerType: "mouse",
      relatedTarget: document.body,
    });

    // The request is kept, not granted: the reader's next movement is what decides it, and this
    // one lands on the reference where the page left it, so the card they asked for opens on
    // the delay it was already waiting out.
    fireEvent.pointerMove(reference, { clientX: 11, clientY: 31, pointerType: "mouse" });
    act(() => {
      jest.advanceTimersByTime(REF_PREVIEW_OPEN_DELAY_MS);
    });
    expect(screen.getByRole("tooltip").textContent).toContain("CORE-1");

    // And a movement that leaves the reference closes it as any other does, so a card kept
    // under a still pointer never stands over nothing once that pointer has moved. This
    // boundary event carries the place the reader moved to, which is what says it is theirs.
    fireEvent.pointerOut(reference, {
      clientX: 80,
      clientY: 120,
      pointerType: "mouse",
      relatedTarget: document.body,
    });
    act(() => {
      jest.advanceTimersByTime(REF_PREVIEW_CLOSE_DELAY_MS);
    });
    expect(screen.queryByRole("tooltip")).toBeNull();
  } finally {
    restoreThePage();
    view.unmount();
    getIssue.mockRestore();
  }
});

test("a reference the page moves in the moment the pointer reaches it still opens its card", () => {
  // A reader reaches a reference by moving onto it, and that movement lands milliseconds before
  // the page draws whatever slides the reference away - which is `e2e/hover-preview.e2e.ts`'s
  // ordinary conversation page in CI's container (run 42628596511, shard 2, red on both
  // attempts: the card never opened). Nothing about the arrival says the reader is leaving, and
  // no clock can tell the two apart, so what the host reads is the pointer's own place.
  const getIssue = spyOn(api, "getIssue").mockImplementation(() => new Promise(() => {}));
  jest.useFakeTimers();
  const view = render(<Page />);
  // The page moves the reference by less than its own height here, so the pointer the reader
  // left at 10, 30 is still on it when the delay runs out.
  const restoreThePage = thePageDraws(screen.getByRole("link", { name: "the reference" }), {
    bottom: 36,
    left: 0,
    right: 100,
    top: 16,
  });
  try {
    const reference = screen.getByRole("link", { name: "the reference" });
    // The reader's arrival, at a place of its own: the host tells a pointer the reader moved
    // from one the engine moved by the position changing, and user-event's hover sends neither
    // those coordinates nor the `pointerType` the hover-intent gate reads.
    fireEvent.pointerMove(reference, { clientX: 10, clientY: 30, pointerType: "mouse" });
    // The hover itself, for that same `pointerType`.
    fireEvent.pointerOver(reference, { pointerType: "mouse" });

    // The page, not the reader, taking the reference out from under the pointer, in the same
    // instant their arrival landed: no pointer movement of user-event's can send a boundary
    // event with its `relatedTarget` while the pointer stays where the reader left it, which is
    // the whole state this row sets up.
    fireEvent.pointerOut(reference, {
      clientX: 10,
      clientY: 30,
      pointerType: "mouse",
      relatedTarget: document.body,
    });

    act(() => {
      jest.advanceTimersByTime(REF_PREVIEW_OPEN_DELAY_MS);
    });
    expect(screen.getByRole("tooltip").textContent).toContain("CORE-1");
  } finally {
    restoreThePage();
    view.unmount();
    getIssue.mockRestore();
  }
});

test("a reference the page had already moved when its host mounted keeps the request the resting pointer made", () => {
  // The host is loaded beside the route rather than in the entry chunk
  // (`features/shell/deferred-shell.ts`), so it can mount in the very commit that moves the
  // reference: it reads the pointer where the browser keeps it - the hover chain - and makes the
  // request for a reference already standing where the page has just put it. The boundary event
  // that follows is the browser settling `:hover` on what is under the pointer now, and the
  // anchor's measured box says nothing, because the page has finished moving it.
  //
  // That is run 42743455211's one red attempt of `e2e/hover-preview.e2e.ts:72` (job
  // 129590901337, flaky): the card's own reads went out 57 ms after the hover action, in the
  // commit that landed the issue header and the margin and took the reference 11 px up from
  // under a pointer resting at its middle, and no card ever opened.
  //
  // Nothing of this pointer has been seen to move, so there is no place to ask the page about
  // and the card opens on its delay: the hover chain has just said the pointer is on the
  // reference, which is the browser's own answer to the same question.
  //
  // happy-dom keeps no hover chain - no pointer moves in this process - so the chain is what
  // this row stands in for (`RefPreview.catch-up.test.tsx` says the same of the focus half,
  // which needs no stand-in).
  const getIssue = spyOn(api, "getIssue").mockImplementation(() => new Promise(() => {}));
  jest.useFakeTimers();
  const view = render(<Page host={false} />);
  const selectAll = document.querySelectorAll.bind(document);
  const hoverChain = spyOn(document, "querySelectorAll");
  try {
    const reference = screen.getByRole("link", { name: "the reference" });
    hoverChain.mockImplementation((selector: string) =>
      selector === ":hover" ? ([reference] as unknown as NodeListOf<Element>) : selectAll(selector)
    );
    view.rerender(<Page />);
    hoverChain.mockRestore();

    // The page has already moved the reference; the pointer has not moved since, and nothing of
    // it has been seen to move at all. This is the browser settling the hover chain afterwards.
    fireEvent.pointerOut(reference, { pointerType: "mouse", relatedTarget: document.body });

    act(() => {
      jest.advanceTimersByTime(REF_PREVIEW_OPEN_DELAY_MS);
    });
    expect(screen.getByRole("tooltip").textContent).toContain("CORE-1");
  } finally {
    hoverChain.mockRestore();
    view.unmount();
    getIssue.mockRestore();
  }
});

test("a dialog drawn over the reference while its delay runs leaves the card closed", () => {
  // A request kept under a still pointer is kept through whatever the page does, and what the
  // page does may be to open a dialog, the inbox peek or a menu across the reference. The
  // reference is still where the pointer is, but it is no longer what the pointer is on, and a
  // card drawn then previews a link the reader cannot see, over the thing they can.
  const getIssue = spyOn(api, "getIssue").mockImplementation(() => new Promise(() => {}));
  jest.useFakeTimers();
  const view = render(<Page />);
  const dialog = document.createElement("div");
  document.body.append(dialog);
  // The reference has not moved at all - the dialog is simply over it at the pointer's place.
  const restoreThePage = thePageDraws(
    screen.getByRole("link", { name: "the reference" }),
    { bottom: 40, left: 0, right: 100, top: 20 },
    dialog
  );
  try {
    const reference = screen.getByRole("link", { name: "the reference" });
    // The reader's arrival and the hover, for the reasons the first row of this file gives:
    // user-event sends neither the pointer's own coordinates nor a `pointerType`.
    fireEvent.pointerMove(reference, { clientX: 10, clientY: 30, pointerType: "mouse" });
    fireEvent.pointerOver(reference, { pointerType: "mouse" });

    // The dialog taking the pointer: the boundary event is at the place the reader left it, so
    // the request is the page's to keep, not theirs to lose.
    fireEvent.pointerOut(reference, {
      clientX: 10,
      clientY: 30,
      pointerType: "mouse",
      relatedTarget: dialog,
    });

    act(() => {
      jest.advanceTimersByTime(REF_PREVIEW_OPEN_DELAY_MS);
    });
    expect(screen.queryByRole("tooltip")).toBeNull();
  } finally {
    restoreThePage();
    dialog.remove();
    view.unmount();
    getIssue.mockRestore();
  }
});

test("a reference the page slid out from under the pointer opens after the delay that follows the reader's next movement onto it", () => {
  // The trace's own case (run 42743455211 above): the page took the reference 11 px up and left
  // the pointer just below it. The request stands - nothing of the reader's took it away - but
  // the reference is not what they are pointing at, so the delay running out draws nothing. The
  // movement that puts their pointer back on it starts that delay again rather than opening the
  // card: a pointer arriving on a reference from over something else is the hover the delay
  // exists to tell from a reader passing through.
  const getIssue = spyOn(api, "getIssue").mockImplementation(() => new Promise(() => {}));
  jest.useFakeTimers();
  const view = render(<Page />);
  // Where the page left the reference: above the pointer the reader rests at 10, 30.
  const restoreThePage = thePageDraws(screen.getByRole("link", { name: "the reference" }), {
    bottom: 19,
    left: 0,
    right: 100,
    top: 3,
  });
  try {
    const reference = screen.getByRole("link", { name: "the reference" });
    // The reader's arrival, the hover, and the page taking the reference away in the same
    // instant - each for the reasons the first row of this file gives: the pointer's own
    // coordinates, the `pointerType` the gate reads, and a boundary event with a
    // `relatedTarget` while the pointer stays where the reader left it, which user-event has no
    // form for.
    fireEvent.pointerMove(reference, { clientX: 10, clientY: 30, pointerType: "mouse" });
    fireEvent.pointerOver(reference, { pointerType: "mouse" });
    fireEvent.pointerOut(reference, {
      clientX: 10,
      clientY: 30,
      pointerType: "mouse",
      relatedTarget: document.body,
    });

    act(() => {
      jest.advanceTimersByTime(REF_PREVIEW_OPEN_DELAY_MS);
    });
    expect(screen.queryByRole("tooltip")).toBeNull();

    // The reader moving onto the reference where the page put it: the card is not there on that
    // movement, and it is there once the delay that movement started has run.
    fireEvent.pointerMove(reference, { clientX: 10, clientY: 10, pointerType: "mouse" });
    expect(screen.queryByRole("tooltip")).toBeNull();
    act(() => {
      jest.advanceTimersByTime(REF_PREVIEW_OPEN_DELAY_MS);
    });
    expect(screen.getByRole("tooltip").textContent).toContain("CORE-1");
  } finally {
    restoreThePage();
    view.unmount();
    getIssue.mockRestore();
  }
});
