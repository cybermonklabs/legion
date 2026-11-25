import { expect, mock, test } from "bun:test";
// biome-ignore lint/style/noRestrictedImports: the pointer sites below need a pointer event user-event cannot send (a pointerType, a held button, a relatedTarget).
import { act, fireEvent, render, screen } from "@testing-library/react";

import { caretAt } from "../../__tests__/caret";
import { CardAsked } from "./RefPreviewLayer";
import { NO_HOVER_PREVIEW_ATTRIBUTE } from "./ref-preview-card";

/** The mark the new issue page's frame carries: the references inside it open no card on hover,
 *  and the caret in its editor's link opens one. */
const NO_HOVER_FRAME = { [NO_HOVER_PREVIEW_ATTRIBUTE]: "" };

/** A page under the listener a failed layer draws: a reference on an ordinary page, with an
 *  editor whose link opens no card for the caret; and the issue page's frame, with a reference
 *  and an editor's link the caret opens a card for. */
function drawPage(ask: () => void) {
  return render(
    <>
      <a data-dispatch-ref="dispatch://CORE-1" href="/issues/CORE-1">
        <span>first</span>
      </a>
      <button type="button">plain</button>
      <div contentEditable suppressContentEditableWarning>
        See{" "}
        <a data-dispatch-href="dispatch://CORE-2" href="/issues/CORE-2">
          the page's editor link
        </a>{" "}
        here.
      </div>
      <div {...NO_HOVER_FRAME}>
        <a data-dispatch-ref="dispatch://CORE-3" href="/issues/CORE-3">
          framed
        </a>
        <div contentEditable suppressContentEditableWarning>
          See{" "}
          <a data-dispatch-href="dispatch://CORE-4" href="/issues/CORE-4">
            the frame's editor link
          </a>{" "}
          here.
        </div>
      </div>
      <CardAsked ask={ask} />
    </>
  );
}

// A failed layer asks for its chunk again for a hover over a marked reference, and for no other
// pointer: the card opens none for a touch, a held button, a move inside the same reference, or
// a reference on the issue page, so none of those asks. A reference whose address the card cannot
// parse still asks, since only the card's chunk parses it, and the reader did point at a reference.
test("a pointer over a reference asks for a card, and a touch, a held button, a move inside it or a reference that opens no card on hover does not", () => {
  const ask = mock(() => undefined);
  const view = drawPage(ask);
  try {
    const first = screen.getByRole("link", { name: "first" });
    const plain = screen.getByRole("button", { name: "plain" });
    // A hover-intent pointer: user-event's hover names no pointerType, which this site needs.
    fireEvent.pointerOver(first, { pointerType: "mouse", relatedTarget: plain });
    expect(ask).toHaveBeenCalledTimes(1);
    // A move from the reference onto a node inside it: user-event cannot name the relatedTarget.
    fireEvent.pointerOver(screen.getByText("first"), {
      pointerType: "mouse",
      relatedTarget: first,
    });
    // A touch pointer: user-event's hover is a mouse, with no pointerType to make it a touch.
    fireEvent.pointerOver(first, { pointerType: "touch", relatedTarget: plain });
    // A held button: user-event's hover carries no `buttons`, which a drag-sweep needs.
    fireEvent.pointerOver(first, { buttons: 1, pointerType: "mouse", relatedTarget: plain });
    // A reference on the issue page: a hover-intent pointer, as above.
    fireEvent.pointerOver(screen.getByRole("link", { name: "framed" }), { pointerType: "mouse" });
    // Something that is no reference: a hover-intent pointer, as above.
    fireEvent.pointerOver(plain, { pointerType: "mouse" });
    expect(ask).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
  }
});

// The keyboard opens a card on a reference anywhere, the issue page included, so its focus asks.
test("a keyboard focus on a reference asks for a card, on the issue page too, and a focus elsewhere does not", () => {
  const ask = mock(() => undefined);
  const view = drawPage(ask);
  try {
    act(() => {
      screen.getByRole("link", { name: "first" }).focus();
    });
    expect(ask).toHaveBeenCalledTimes(1);
    act(() => {
      screen.getByRole("link", { name: "framed" }).focus();
    });
    expect(ask).toHaveBeenCalledTimes(2);
    act(() => {
      screen.getByRole("button", { name: "plain" }).focus();
    });
    expect(ask).toHaveBeenCalledTimes(2);
  } finally {
    view.unmount();
  }
});

// A link in an editor takes no focus, so on the issue page the caret entering it is the keyboard
// asking for its card. A caret that moves on inside the link asks nothing more, and the caret in
// an editor's link elsewhere asks nothing: a hover opens that one's card.
test("the caret entering an editor's link on the issue page asks for a card once, and the caret in an editor's link elsewhere does not", () => {
  const ask = mock(() => undefined);
  const view = drawPage(ask);
  try {
    const framed = screen.getByText("the frame's editor link").firstChild as Node;
    act(() => {
      caretAt(framed, 2);
    });
    expect(ask).toHaveBeenCalledTimes(1);
    act(() => {
      caretAt(framed, 4);
    });
    expect(ask).toHaveBeenCalledTimes(1);
    act(() => {
      caretAt(screen.getByText("the page's editor link").firstChild as Node, 2);
    });
    expect(ask).toHaveBeenCalledTimes(1);
    act(() => {
      caretAt(framed, 3);
    });
    expect(ask).toHaveBeenCalledTimes(2);
  } finally {
    view.unmount();
    // A browser moves the caret out of nodes that leave the page, and the test DOM keeps it in
    // them: a listener that a later test draws would read it there.
    document.getSelection()?.removeAllRanges();
  }
});

// The boundary draws the layer again for an ask, and the listener goes with the fallback: a
// listener left behind would ask again for every card the layer that came down opens itself.
test("the listener asks for nothing once the boundary no longer draws it", () => {
  const ask = mock(() => undefined);
  const reference = (
    <a data-dispatch-ref="dispatch://CORE-1" href="/issues/CORE-1">
      first
    </a>
  );
  const view = render(
    <>
      {reference}
      <CardAsked ask={ask} />
    </>
  );
  try {
    view.rerender(reference);
    // A hover-intent pointer: user-event's hover names no pointerType, which this site needs.
    fireEvent.pointerOver(screen.getByRole("link", { name: "first" }), { pointerType: "mouse" });
    expect(ask).not.toHaveBeenCalled();
  } finally {
    view.unmount();
  }
});
