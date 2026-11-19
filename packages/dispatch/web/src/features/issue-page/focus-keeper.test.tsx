import { expect, mock, test } from "bun:test";
// biome-ignore lint/style/noRestrictedImports: the press case needs a press, a blur and a removal inside one task, and user-event's press awaits between its events; the tap cases need a tap's order, its pointer up before the blur its mousedown makes, and user-event moves the focus as its pointer goes down; the cancelled, right-button, clickless and window-blur press cases need a pointercancel, a press of another button that moves no focus, a contextmenu with no pointerup, a pointerup with no click after it, and a blur of the window with a press under way; the press reader's own cases need each way a press ends, a dragend and the window's blur among them, alone after a bare pointerdown, which user-event has no form for.
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";

import { jump, onFakeClock } from "../../__tests__/fake-clock";
import { userOnFakeClock } from "../../__tests__/user";
import { emulateChromiumFocusFixup } from "./chromium-focus-fixup";
import { type FocusFallback, hearPresses, useFocusKeeper } from "./focus-keeper";
import { DETAILS_ATTRIBUTE } from "./margin-slot";

/** What has become of the region's Save control: drawn, disabled, removed, or drawn anew under
 *  another key, which React draws as a new element in the old one's place. */
type SaveControl = "drawn" | "disabled" | "removed" | "replaced";

/**
 * A region whose focus the keeper keeps: a Save control the props disable, remove or replace, a
 * sibling the props remove, a Details button whose press keeps the focus where it is, as the
 * header's Details control does, and two fallback buttons. The first fallback finds Save's
 * replacement among what the change added; the other two are the fallback buttons, in order.
 */
function Region({
  firstFallbackDisabled = false,
  save = "drawn",
  sibling = true,
}: {
  firstFallbackDisabled?: boolean;
  save?: SaveControl;
  sibling?: boolean;
}) {
  const region = useRef<HTMLDivElement>(null);
  // A new array each render, as a caller writes it.
  useFocusKeeper(region, [
    ({ added, lost }) => added.find((element) => element.textContent === lost.textContent),
    ({ root }) => root.querySelector<HTMLElement>("[data-fallback='first']"),
    ({ root }) => root.querySelector<HTMLElement>("[data-fallback='second']"),
  ]);
  return (
    <div ref={region}>
      {save === "removed" ? null : (
        <button
          disabled={save === "disabled"}
          key={save === "replaced" ? "anew" : "first"}
          type="button"
        >
          Save
        </button>
      )}
      {sibling ? <button type="button">Sibling</button> : null}
      <button onMouseDown={(event) => event.preventDefault()} type="button">
        Details
      </button>
      <button data-fallback="first" disabled={firstFallbackDisabled} type="button">
        First fallback
      </button>
      <button data-fallback="second" type="button">
        Second fallback
      </button>
    </div>
  );
}

function button(name: string): HTMLElement {
  return screen.getByRole("button", { name });
}

test("a focused control that turns disabled hands the focus to the first fallback", async () => {
  const stopFixup = emulateChromiumFocusFixup();
  const view = render(<Region />);
  try {
    act(() => button("Save").focus());
    view.rerender(<Region save="disabled" />);
    await waitFor(() => expect(button("First fallback")).toHaveFocus());
  } finally {
    view.unmount();
    stopFixup();
  }
});

test("a focused control that is removed hands the focus to the first fallback", async () => {
  const view = render(<Region />);
  try {
    act(() => button("Save").focus());
    view.rerender(<Region save="removed" />);
    await waitFor(() => expect(button("First fallback")).toHaveFocus());
  } finally {
    view.unmount();
  }
});

test("a focused control drawn anew hands the focus to the replacement a fallback finds", async () => {
  const view = render(<Region />);
  try {
    const before = button("Save");
    act(() => before.focus());
    view.rerender(<Region save="replaced" />);
    // Only the fallback that reads the change's added elements finds the new Save: without them,
    // the focus would land on the first fallback button.
    await waitFor(() => expect(button("Save")).toHaveFocus());
    expect(button("Save")).not.toBe(before);
  } finally {
    view.unmount();
  }
});

test("a fallback that cannot hold the focus is passed over for the next one", async () => {
  const view = render(<Region firstFallbackDisabled />);
  try {
    act(() => button("Save").focus());
    view.rerender(<Region firstFallbackDisabled save="removed" />);
    await waitFor(() => expect(button("Second fallback")).toHaveFocus());
  } finally {
    view.unmount();
  }
});

test("a focus the reader put on the body stays there when the control is then disabled", async () => {
  const stopFixup = emulateChromiumFocusFixup();
  const view = render(<Region />);
  try {
    await onFakeClock(async () => {
      act(() => button("Save").focus());
      act(() => button("Save").blur());
      // The keeper's check of the blur runs here.
      await act(async () => {});
      view.rerender(<Region save="disabled" />);
      // A move the keeper made of a loss it read would run one task after its check.
      await jump(1);
      expect(document.body).toHaveFocus();
    });
  } finally {
    view.unmount();
    stopFixup();
  }
});

test("a focus on a control stays there when a sibling is removed", async () => {
  const view = render(<Region />);
  try {
    await onFakeClock(async () => {
      act(() => button("Save").focus());
      view.rerender(<Region sibling={false} />);
      await jump(1);
      expect(button("Save")).toHaveFocus();
    });
  } finally {
    view.unmount();
  }
});

test("a press on the page is the reader's move, even when the control then goes", async () => {
  const view = render(<Region />);
  try {
    await onFakeClock(async () => {
      act(() => button("Save").focus());
      // A press on a blank part of the page blurs the control, and the blur's own handler may
      // remove it before the keeper's check runs. user-event cannot hold all three in one task:
      // its press awaits between its events, and the keeper's check would run in that gap.
      fireEvent.pointerDown(document.body);
      act(() => button("Save").blur());
      view.rerender(<Region save="removed" />);
      await jump(1);
      expect(document.body).toHaveFocus();
    });
  } finally {
    view.unmount();
  }
});

test("a press on the focused control is no move of the reader's when its write disables it", async () => {
  const user = userEvent.setup();
  const stopFixup = emulateChromiumFocusFixup();
  const view = render(<Region />);
  try {
    act(() => button("Save").focus());
    // The press focuses nothing anew, as the control already holds the focus.
    await user.click(button("Save"));
    view.rerender(<Region save="disabled" />);
    await waitFor(() => expect(button("First fallback")).toHaveFocus());
  } finally {
    view.unmount();
    stopFixup();
  }
});

test("a press that moves no focus is no move of the reader's once they press a key", async () => {
  const user = userEvent.setup();
  const stopFixup = emulateChromiumFocusFixup();
  const view = render(<Region />);
  try {
    act(() => button("Save").focus());
    // Details prevents its mousedown, so the press leaves the focus on Save and no focusin
    // follows it.
    await user.click(button("Details"));
    expect(button("Save")).toHaveFocus();
    // Enter on Save then starts its write, which disables it.
    await user.keyboard("{Enter}");
    view.rerender(<Region save="disabled" />);
    await waitFor(() => expect(button("First fallback")).toHaveFocus());
  } finally {
    view.unmount();
    stopFixup();
  }
});

test("a press that moves no focus is no move of the reader's when a write then disables the control", async () => {
  const user = userEvent.setup();
  const stopFixup = emulateChromiumFocusFixup();
  const view = render(<Region />);
  try {
    act(() => button("Save").focus());
    // Details cancels its mousedown, so the press leaves the focus on Save. Another reader's write
    // then disables Save, with no key pressed in between.
    await user.click(button("Details"));
    expect(button("Save")).toHaveFocus();
    view.rerender(<Region save="disabled" />);
    await waitFor(() => expect(button("First fallback")).toHaveFocus());
  } finally {
    view.unmount();
    stopFixup();
  }
});

test("a touch the browser cancels is no move of the reader's when a write then disables the control", async () => {
  const stopFixup = emulateChromiumFocusFixup();
  const view = render(<Region />);
  try {
    act(() => button("Save").focus());
    // A touch on the page that the browser turns into a scroll: its pointer goes down and the
    // browser cancels it, with no pointerup and no click, and the focus stays on Save. Another
    // reader's write then disables Save. user-event has no form for a cancelled pointer.
    fireEvent.pointerDown(document.body, { button: 0, pointerType: "touch" });
    fireEvent.pointerCancel(document.body, { pointerType: "touch" });
    expect(button("Save")).toHaveFocus();
    view.rerender(<Region save="disabled" />);
    await waitFor(() => expect(button("First fallback")).toHaveFocus());
  } finally {
    view.unmount();
    stopFixup();
  }
});

test("a right press that moves no focus is no move of the reader's when a write then disables the control", async () => {
  const stopFixup = emulateChromiumFocusFixup();
  const view = render(<Region />);
  try {
    act(() => button("Save").focus());
    // A press of the right button on the page goes down and up with the focus still on Save,
    // and no click follows a press of any button but the primary one. Another reader's write
    // then disables Save. user-event moves the focus as its pointer goes down.
    fireEvent.pointerDown(document.body, { button: 2 });
    fireEvent.pointerUp(document.body, { button: 2 });
    expect(button("Save")).toHaveFocus();
    view.rerender(<Region save="disabled" />);
    await waitFor(() => expect(button("First fallback")).toHaveFocus());
  } finally {
    view.unmount();
    stopFixup();
  }
});

test("a right press whose context menu takes its pointerup is no move of the reader's when a write then disables the control", async () => {
  const stopFixup = emulateChromiumFocusFixup();
  const view = render(<Region />);
  try {
    act(() => button("Save").focus());
    // Where the context menu opens as the right button goes down, the menu takes the pointerup,
    // so the page hears the press go down and the contextmenu and nothing after them, with the
    // focus still on Save. Another reader's write then disables Save. user-event sends no
    // contextmenu without a pointerup.
    fireEvent.pointerDown(document.body, { button: 2 });
    fireEvent.contextMenu(document.body, { button: 2 });
    expect(button("Save")).toHaveFocus();
    view.rerender(<Region save="disabled" />);
    await waitFor(() => expect(button("First fallback")).toHaveFocus());
  } finally {
    view.unmount();
    stopFixup();
  }
});

test("a press under way as the window blurs is no move of the reader's when a write then disables the control", async () => {
  const stopFixup = emulateChromiumFocusFixup();
  const view = render(<Region />);
  try {
    act(() => button("Save").focus());
    // A press goes down on the page with the focus still on Save, and the reader leaves the window
    // before its release, which the page may then never hear. The window keeps Save as its focused
    // element as it blurs. Another reader's write then disables Save. user-event has no form for
    // a blur of the window.
    fireEvent.pointerDown(document.body);
    fireEvent.blur(window);
    expect(button("Save")).toHaveFocus();
    view.rerender(<Region save="disabled" />);
    await waitFor(() => expect(button("First fallback")).toHaveFocus());
  } finally {
    view.unmount();
    stopFixup();
  }
});

test("a tap on the page is the reader's move, even when the control then goes", async () => {
  const view = render(<Region />);
  try {
    await onFakeClock(async () => {
      act(() => button("Save").focus());
      // A tap on a blank part of the page: its pointer goes down and up with the focus still on
      // Save, and only the mousedown the browser sends after it blurs Save to the body, before its
      // click. user-event moves the focus as its pointer goes down.
      fireEvent.pointerDown(document.body);
      fireEvent.pointerUp(document.body);
      expect(button("Save")).toHaveFocus();
      act(() => button("Save").blur());
      // The tap's click comes last, on the same blank part of the page, and a write then removes
      // Save, all before the keeper's check of the blur runs.
      fireEvent.click(document.body);
      view.rerender(<Region save="removed" />);
      await jump(1);
      expect(document.body).toHaveFocus();
    });
  } finally {
    view.unmount();
  }
});

/**
 * Hears the reader's presses on the page with spies for what `hearPresses` tells, on a page whose
 * Before holds the focus as each press begins and whose After is where a press that moves the
 * focus moves it. Returns the spies and what ends the hearing and takes the page away.
 */
function hearOnPage() {
  const view = render(
    <>
      <button type="button">Before</button>
      <button type="button">After</button>
    </>
  );
  act(() => button("Before").focus());
  const begin = mock(() => "begun");
  const done = mock(() => undefined);
  const stopHearing = hearPresses(document, begin, done);
  return {
    begin,
    done,
    stop: () => {
      stopHearing();
      view.unmount();
    },
  };
}

// Each way a press is over, after its pointerdown on a blank part of the page with the focus on
// Before: `done` hears of it once, with where the focus was as it began and what `begin` answered.
test.each([
  [
    "its pointerup once it has moved the focus",
    0,
    () => {
      act(() => button("After").focus());
      // A mouse press has moved the focus by its pointerup; user-event moves it as it goes down.
      fireEvent.pointerUp(document.body);
    },
  ],
  // A press of another button sends no click, so its pointerup alone ends it.
  ["the pointerup of a right press", 2, () => fireEvent.pointerUp(document.body, { button: 2 })],
  // A click with no pointerup before it, so that the click alone is what ends the press.
  ["its click", 0, () => fireEvent.click(document.body)],
  // A press the browser cancels, as a touch it turns into a scroll; user-event cancels none.
  ["its pointercancel", 0, () => fireEvent.pointerCancel(document.body)],
  // A press that becomes a drag with no pointercancel; user-event has no form for a drag.
  ["its dragend", 0, () => fireEvent.dragEnd(document.body)],
  // A right press whose context menu takes its pointerup; user-event sends none without one.
  ["the contextmenu of a right press", 2, () => fireEvent.contextMenu(document.body)],
  // The reader leaves the window mid-press; user-event has no form for a blur of the window.
  ["the window's blur", 0, () => fireEvent.blur(window)],
] as const)("a press is over at %s, and done hears where the focus was as it began", (_ending, pressed, end) => {
  const { begin, done, stop } = hearOnPage();
  try {
    // A bare pointerdown, so that the ending alone is what ends the press.
    fireEvent.pointerDown(document.body, { button: pressed });
    expect(begin).toHaveBeenCalledTimes(1);
    expect(done).not.toHaveBeenCalled();
    end();
    expect(done).toHaveBeenCalledTimes(1);
    expect(done).toHaveBeenCalledWith(button("Before"), "begun");
  } finally {
    stop();
  }
});

// A press with no ending yet: `done` hears nothing of it, and its click then ends it as one press.
test.each([
  ["its pointerdown alone", (): void => undefined],
  // A tap moves the focus only after its pointer is up, from the mousedown the browser sends it.
  ["the pointerup of a tap that has moved no focus yet", () => fireEvent.pointerUp(document.body)],
  // A mouse press blurs the focused control as its button goes down: an element's blur, not the
  // window's.
  ["the blur of the control that held the focus", () => act(() => button("Before").blur())],
] as const)("a press is still under way after %s", (_step, step) => {
  const { done, stop } = hearOnPage();
  try {
    // A bare pointerdown of the primary button, which user-event follows with its own events.
    fireEvent.pointerDown(document.body, { button: 0 });
    step();
    expect(done).not.toHaveBeenCalled();
    // The press's click, which user-event sends only after a pointerdown of its own.
    fireEvent.click(document.body);
    expect(done).toHaveBeenCalledTimes(1);
    expect(done).toHaveBeenCalledWith(button("Before"), "begun");
  } finally {
    stop();
  }
});

test("a control that turns disabled gets the focus back once it is enabled again", async () => {
  const stopFixup = emulateChromiumFocusFixup();
  const view = render(<Region />);
  try {
    act(() => button("Save").focus());
    view.rerender(<Region save="disabled" />);
    await waitFor(() => expect(button("First fallback")).toHaveFocus());
    view.rerender(<Region />);
    await waitFor(() => expect(button("Save")).toHaveFocus());
  } finally {
    view.unmount();
    stopFixup();
  }
});

test("a Retry that fails twice has the focus back after each failure", async () => {
  const stopFixup = emulateChromiumFocusFixup();
  const view = render(<Region />);
  try {
    act(() => button("Save").focus());
    // The reader presses Retry: it is disabled while its read runs again, and that read fails.
    view.rerender(<Region save="disabled" />);
    await waitFor(() => expect(button("First fallback")).toHaveFocus());
    view.rerender(<Region />);
    await waitFor(() => expect(button("Save")).toHaveFocus());
    // The reader presses it again, and the read fails again.
    view.rerender(<Region save="disabled" />);
    await waitFor(() => expect(button("First fallback")).toHaveFocus());
    view.rerender(<Region />);
    await waitFor(() => expect(button("Save")).toHaveFocus());
  } finally {
    view.unmount();
    stopFixup();
  }
});

test("a key the reader presses where the keeper put the focus keeps it there", async () => {
  const user = userEvent.setup();
  const stopFixup = emulateChromiumFocusFixup();
  const view = render(<Region />);
  try {
    act(() => button("Save").focus());
    view.rerender(<Region save="disabled" />);
    await waitFor(() => expect(button("First fallback")).toHaveFocus());
    // The reader acts on the control the keeper gave them, inside the region: from then on that
    // is where they are, so Save enabled again takes nothing back.
    await user.keyboard("{Enter}");
    view.rerender(<Region />);
    // A give-back would run in the check this change starts.
    await act(async () => {});
    expect(button("First fallback")).toHaveFocus();
  } finally {
    view.unmount();
    stopFixup();
  }
});

/**
 * A region whose Save control an answer removes, and whose own effect then hands the focus to the
 * second fallback button when the focus went with Save. The answer lands outside any event, as a
 * read's does, so React runs that effect in a task of its own scheduler, after the keeper's
 * observer has heard the removal. `onHandOn` is told once the effect has run.
 */
function HandOnRegion({ answer, onHandOn }: { answer: Promise<void>; onHandOn: () => void }) {
  const region = useRef<HTMLDivElement>(null);
  const second = useRef<HTMLButtonElement>(null);
  const [answered, setAnswered] = useState(false);
  useFocusKeeper(region, [
    ({ root }) => root.querySelector<HTMLElement>("[data-fallback='first']"),
  ]);
  useEffect(() => {
    void answer.then(() => setAnswered(true));
  }, [answer]);
  useEffect(() => {
    if (!answered) return;
    // The site hands the focus on only when it went with Save, never past a place the reader chose.
    if (document.activeElement === document.body) second.current?.focus();
    onHandOn();
  }, [answered, onHandOn]);
  return (
    <div ref={region}>
      {answered ? null : <button type="button">Save</button>}
      <button data-fallback="first" type="button">
        First fallback
      </button>
      <button ref={second} type="button">
        Second fallback
      </button>
    </div>
  );
}

/** React's switch for its warning about an update outside `act`, which Testing Library's own waits
 *  turn off while they let the page's tasks run. */
const reactGlobals = globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean };

test("a site that hands the focus on from a passive effect keeps its own choice", async () => {
  const answer = Promise.withResolvers<void>();
  const handedOn = Promise.withResolvers<void>();
  const view = render(<HandOnRegion answer={answer.promise} onHandOn={handedOn.resolve} />);
  const actEnvironment = reactGlobals.IS_REACT_ACT_ENVIRONMENT;
  // A held clock keeps the keeper's move, which waits one task, from running until the site's
  // effect has run. React runs that effect from its own scheduler, which the clock does not hold;
  // on a real clock, which of the two tasks ran first would depend on how busy the machine is. A
  // keeper that moved at once would move in its observer's microtask, before either.
  try {
    await onFakeClock(async () => {
      act(() => button("Save").focus());
      // The answer lands outside `act`, so React draws its change and runs the effect in tasks of
      // its scheduler as a browser does, rather than all at once as `act` ends.
      reactGlobals.IS_REACT_ACT_ENVIRONMENT = false;
      answer.resolve();
      await handedOn.promise;
      reactGlobals.IS_REACT_ACT_ENVIRONMENT = actEnvironment;
    });
    // The keeper's move ran as the real clock came back, and found the focus already handed on.
    expect(button("Second fallback")).toHaveFocus();
  } finally {
    reactGlobals.IS_REACT_ACT_ENVIRONMENT = actEnvironment;
    view.unmount();
  }
});

/** The fallback of a region whose control stands in a box (`data-box`): the box's first button,
 *  found from where the lost control stood. */
const firstInBox: FocusFallback = ({ place }) =>
  place?.closest("[data-box]")?.querySelector("button");

/** A box whose Act control is drawn inside a wrapper of its own, which goes with it, beside the
 *  box's Chip. The keeper's one fallback finds the box from where Act stood. */
function BoxRegion({ shown = true }: { shown?: boolean }) {
  const region = useRef<HTMLDivElement>(null);
  useFocusKeeper(region, [firstInBox]);
  return (
    <div ref={region}>
      <div data-box>
        {shown ? (
          <span>
            <button type="button">Act</button>
          </span>
        ) : null}
        <button type="button">Chip</button>
      </div>
    </div>
  );
}

test("a control removed with its wrapper hands the focus to the one beside where it stood", async () => {
  const view = render(<BoxRegion />);
  try {
    act(() => button("Act").focus());
    view.rerender(<BoxRegion shown={false} />);
    await waitFor(() => expect(button("Chip")).toHaveFocus());
  } finally {
    view.unmount();
  }
});

/** What has become of the layer: open, closed by a `hidden` on it as Details closes, or closed by
 *  its removal. */
type LayerState = "open" | "hidden" | "removed";

/**
 * A popover a control of Details opens, as Edit labels opens its list: drawn on the page's body,
 * outside the layer and the region, it takes the focus as it opens, and its Done closes it and
 * gives the focus back to the control that opened it. A press outside it closes it with no focus
 * given back, as an `AnchoredPanel` closes on a pointerdown outside itself.
 */
function Popover({ onDismiss, onDone }: { onDismiss: () => void; onDone: () => void }) {
  const panel = useRef<HTMLDivElement>(null);
  const first = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    first.current?.focus();
  }, []);
  useEffect(() => {
    const outside = (event: PointerEvent) => {
      if (event.target instanceof Node && panel.current?.contains(event.target)) return;
      onDismiss();
    };
    document.addEventListener("pointerdown", outside);
    return () => document.removeEventListener("pointerdown", outside);
  }, [onDismiss]);
  return createPortal(
    <div ref={panel}>
      <button ref={first} type="button">
        Label one
      </button>
      <button type="button">Label two</button>
      <button onClick={onDone} type="button">
        Done
      </button>
    </div>,
    document.body
  );
}

/**
 * A region holding an Opener, a Details control and the two fallback buttons, and beside it,
 * outside the region, a Load more of the page, a search palette and a layer: Details' `aside`,
 * holding a Close as Details holds its own, an Edit labels that opens a popover, and inside it a
 * dialog of its own drawn as an `aside`, holding a Field. The palette is a dialog the app draws
 * in its root, as the layer and the region are drawn, which takes the focus as it opens and gives
 * it back as it shuts. The Details control hides the layer, and cancels its mousedown so a press
 * of it leaves the focus where it is, as the header's own does. The props remove the Opener, as a
 * write does while the layer is open; draw the Load more; draw the palette; remove the Close, the
 * layer's own control; and close the layer.
 */
function LayerRegion({
  close = true,
  layer = "open",
  loadMore = false,
  opener = true,
  palette = false,
}: {
  close?: boolean;
  layer?: LayerState;
  loadMore?: boolean;
  opener?: boolean;
  palette?: boolean;
}) {
  const region = useRef<HTMLDivElement>(null);
  const editLabels = useRef<HTMLButtonElement>(null);
  const [popover, setPopover] = useState(false);
  const [shut, setShut] = useState(false);
  useFocusKeeper(region, [
    ({ root }) => root.querySelector<HTMLElement>("[data-fallback='first']"),
    ({ root }) => root.querySelector<HTMLElement>("[data-fallback='second']"),
  ]);
  return (
    <>
      <div ref={region}>
        {opener ? <button type="button">Opener</button> : null}
        <button
          onClick={() => setShut(true)}
          onMouseDown={(event) => event.preventDefault()}
          type="button"
        >
          Details
        </button>
        <button data-fallback="first" type="button">
          First fallback
        </button>
        <button data-fallback="second" type="button">
          Second fallback
        </button>
      </div>
      {loadMore ? <button type="button">Load more</button> : null}
      {layer === "removed" ? null : (
        <aside hidden={layer === "hidden" || shut} {...{ [DETAILS_ATTRIBUTE]: "" }}>
          {close ? <button type="button">Close</button> : null}
          <button onClick={() => setPopover(true)} ref={editLabels} type="button">
            Edit labels
          </button>
          {popover ? (
            <Popover
              onDismiss={() => setPopover(false)}
              onDone={() => {
                editLabels.current?.focus();
                setPopover(false);
              }}
            />
          ) : null}
          <aside role="dialog">
            <button type="button">Field</button>
          </aside>
        </aside>
      )}
      {palette ? (
        <div aria-label="Search" role="dialog">
          <button type="button">Search</button>
        </div>
      ) : null}
    </>
  );
}

/** Focuses the Opener and then the layer's Close, as Details takes the focus as it opens, and
 *  lets the keeper's check of the focus leaving the region run. */
async function openLayer(): Promise<void> {
  act(() => button("Opener").focus());
  act(() => button("Close").focus());
  await act(async () => {});
}

test("a layer that closes after a write removed its opener hands the focus to the first fallback", async () => {
  const view = render(<LayerRegion />);
  try {
    await onFakeClock(async () => {
      await openLayer();
      view.rerender(<LayerRegion opener={false} />);
      // One task after the write the layer is still open, and the focus stays on its Close.
      await jump(1);
      expect(button("Close")).toHaveFocus();
      // The layer closes, and its Close takes the focus with it to the body.
      view.rerender(<LayerRegion layer="removed" opener={false} />);
      expect(document.body).toHaveFocus();
      await jump(1);
      expect(button("First fallback")).toHaveFocus();
    });
  } finally {
    view.unmount();
  }
});

test("a layer that closes while its opener stays moves no focus", async () => {
  const view = render(<LayerRegion />);
  try {
    await onFakeClock(async () => {
      await openLayer();
      // Nothing here gives the focus back as the layer closes, so it is left on the body: the
      // opener is still there to be given it, which is the layer's to do.
      view.rerender(<LayerRegion layer="removed" />);
      await jump(1);
      expect(document.body).toHaveFocus();
    });
  } finally {
    view.unmount();
  }
});

test("a press on the page before the layer closes is the reader's move, though its opener is gone", async () => {
  const view = render(<LayerRegion />);
  try {
    await onFakeClock(async () => {
      const user = userOnFakeClock();
      await openLayer();
      view.rerender(<LayerRegion opener={false} />);
      await jump(1);
      // A press on a blank part of the page, outside the layer, puts the focus on the body.
      await user.click(document.body);
      expect(document.body).toHaveFocus();
      view.rerender(<LayerRegion layer="removed" opener={false} />);
      await jump(1);
      expect(document.body).toHaveFocus();
    });
  } finally {
    view.unmount();
  }
});

test("a layer that closes after a press on the page and a focus back in the layer hands the focus to the first fallback", async () => {
  const view = render(<LayerRegion />);
  try {
    await onFakeClock(async () => {
      const user = userOnFakeClock();
      await openLayer();
      view.rerender(<LayerRegion opener={false} />);
      await jump(1);
      await user.click(document.body);
      expect(document.body).toHaveFocus();
      // The reader comes back into the layer, so the press no longer put the focus where it is.
      act(() => button("Close").focus());
      view.rerender(<LayerRegion layer="removed" opener={false} />);
      await jump(1);
      expect(button("First fallback")).toHaveFocus();
    });
  } finally {
    view.unmount();
  }
});

test("a focus lost inside a layer still open stays where it fell, though its opener is gone", async () => {
  const view = render(<LayerRegion />);
  try {
    await onFakeClock(async () => {
      await openLayer();
      view.rerender(<LayerRegion opener={false} />);
      await jump(1);
      // The layer's own control goes while it holds the focus, and the layer stays open.
      view.rerender(<LayerRegion close={false} opener={false} />);
      await jump(1);
      expect(document.body).toHaveFocus();
    });
  } finally {
    view.unmount();
  }
});

test("a layer hidden with the focus still on its Close hands the focus to the first fallback", async () => {
  const view = render(<LayerRegion />);
  try {
    await onFakeClock(async () => {
      await openLayer();
      view.rerender(<LayerRegion opener={false} />);
      await jump(1);
      // Details closes by hiding its aside, and Chromium drops the focus off the hidden Close to
      // the body only some tasks later: one task on, the focus is still on the Close.
      view.rerender(<LayerRegion layer="hidden" opener={false} />);
      expect(screen.getByRole("button", { hidden: true, name: "Close" })).toHaveFocus();
      await jump(1);
      expect(button("First fallback")).toHaveFocus();
    });
  } finally {
    view.unmount();
  }
});

test("a layer closed after the focus left a dialog inside it for the layer's Close hands the focus to the first fallback", async () => {
  const view = render(<LayerRegion />);
  try {
    await onFakeClock(async () => {
      // The focus goes from the Opener into the dialog inside Details, and the keeper's check of
      // the focus leaving the region runs.
      act(() => button("Opener").focus());
      act(() => button("Field").focus());
      await act(async () => {});
      // The focus leaves that dialog for Details' own Close: the reader is still in Details.
      act(() => button("Close").focus());
      view.rerender(<LayerRegion opener={false} />);
      await jump(1);
      expect(button("Close")).toHaveFocus();
      // Details closes by hiding its aside, with the focus still on its Close.
      view.rerender(<LayerRegion layer="hidden" opener={false} />);
      await jump(1);
      expect(button("First fallback")).toHaveFocus();
    });
  } finally {
    view.unmount();
  }
});

test("a layer that closes after a popover it opened gave the focus back hands the focus to the first fallback", async () => {
  const view = render(<LayerRegion />);
  try {
    await onFakeClock(async () => {
      const user = userOnFakeClock();
      await openLayer();
      // Edit labels opens its popover, which takes the focus outside the layer and the region.
      await user.click(button("Edit labels"));
      expect(button("Label one")).toHaveFocus();
      // The reader presses another control of the popover, and a write removes the Opener.
      await user.click(button("Label two"));
      view.rerender(<LayerRegion opener={false} />);
      await jump(1);
      // Done closes the popover and gives the focus back into the layer.
      await user.click(button("Done"));
      expect(button("Edit labels")).toHaveFocus();
      view.rerender(<LayerRegion layer="hidden" opener={false} />);
      await jump(1);
      expect(button("First fallback")).toHaveFocus();
    });
  } finally {
    view.unmount();
  }
});

test("a layer that closes with the focus in a popover it opened hands the focus to the first fallback", async () => {
  const view = render(<LayerRegion />);
  try {
    await onFakeClock(async () => {
      const user = userOnFakeClock();
      await openLayer();
      await user.click(button("Edit labels"));
      // A press inside the popover moves the focus within it, outside the layer: it puts no
      // focus on the page.
      await user.click(button("Label two"));
      view.rerender(<LayerRegion opener={false} />);
      await jump(1);
      expect(button("Label two")).toHaveFocus();
      // The layer closes with the focus still in its popover, which goes with it.
      view.rerender(<LayerRegion layer="removed" opener={false} />);
      expect(document.body).toHaveFocus();
      await jump(1);
      expect(button("First fallback")).toHaveFocus();
    });
  } finally {
    view.unmount();
  }
});

test("a layer that closes while a popover it opened keeps the focus moves no focus, though its opener is gone", async () => {
  const view = render(<LayerRegion />);
  try {
    await onFakeClock(async () => {
      const user = userOnFakeClock();
      await openLayer();
      await user.click(button("Edit labels"));
      expect(button("Label one")).toHaveFocus();
      view.rerender(<LayerRegion opener={false} />);
      await jump(1);
      // Details hides, and the popover, drawn on the page's body, stays open with the focus.
      view.rerender(<LayerRegion layer="hidden" opener={false} />);
      await jump(1);
      expect(button("Label one")).toHaveFocus();
    });
  } finally {
    view.unmount();
  }
});

test("a press that keeps the focus where it is and closes the layer is no move of the reader's, though its opener is gone", async () => {
  const view = render(<LayerRegion />);
  try {
    await onFakeClock(async () => {
      const user = userOnFakeClock();
      await openLayer();
      view.rerender(<LayerRegion opener={false} />);
      await jump(1);
      // The press on Details, outside the layer, leaves the focus on the layer's Close, and its
      // click hides the layer.
      await user.click(button("Details"));
      await jump(1);
      expect(button("First fallback")).toHaveFocus();
    });
  } finally {
    view.unmount();
  }
});

// The same press, made after the focus fell to the body inside the open layer: a press on a blank
// part of the layer puts the focus there, which is no move onto the page. The press on Details
// that follows moves no focus, so it is no move of the reader's either, and the layer's close
// still owes the reader a place.
test("a press that moves no focus from the body, after a press inside the layer, is no move of the reader's, though its opener is gone", async () => {
  const view = render(<LayerRegion />);
  try {
    await onFakeClock(async () => {
      const user = userOnFakeClock();
      await openLayer();
      view.rerender(<LayerRegion opener={false} />);
      await jump(1);
      const layer = document.querySelector<HTMLElement>(`aside[${DETAILS_ATTRIBUTE}]`);
      if (layer === null) throw new Error("the layer is not drawn");
      await user.click(layer);
      expect(document.body).toHaveFocus();
      await user.click(button("Details"));
      await jump(1);
      expect(button("First fallback")).toHaveFocus();
    });
  } finally {
    view.unmount();
  }
});

test("a press that shuts a popover as it begins, keeps the focus where it fell and closes the layer is no move of the reader's, though its opener is gone", async () => {
  const view = render(<LayerRegion />);
  try {
    await onFakeClock(async () => {
      const user = userOnFakeClock();
      await openLayer();
      await user.click(button("Edit labels"));
      expect(button("Label one")).toHaveFocus();
      view.rerender(<LayerRegion opener={false} />);
      await jump(1);
      // The press on Details shuts the popover at its pointerdown, which takes Label one along and
      // drops the focus to the body before the press's mousedown; Details then moves no focus and
      // hides the layer from its click.
      await user.click(button("Details"));
      await jump(1);
      expect(button("First fallback")).toHaveFocus();
    });
  } finally {
    view.unmount();
  }
});

test("a tap on the page before the layer closes is the reader's move, though its opener is gone", async () => {
  const view = render(<LayerRegion />);
  try {
    await onFakeClock(async () => {
      await openLayer();
      view.rerender(<LayerRegion opener={false} />);
      await jump(1);
      // A tap on a blank part of the page: its pointer goes down and up with the focus still on
      // the layer's Close, and only the mousedown the browser sends after it blurs the Close to
      // the body, before its click. user-event moves the focus as its pointer goes down.
      fireEvent.pointerDown(document.body);
      fireEvent.pointerUp(document.body);
      expect(button("Close")).toHaveFocus();
      act(() => button("Close").blur());
      // The tap's click comes last, on the same blank part of the page.
      fireEvent.click(document.body);
      view.rerender(<LayerRegion layer="removed" opener={false} />);
      await jump(1);
      expect(document.body).toHaveFocus();
    });
  } finally {
    view.unmount();
  }
});

test("a press on the page that moved the focus by its pointerup is the reader's move with no click after it, though its opener is gone", async () => {
  const view = render(<LayerRegion />);
  try {
    await onFakeClock(async () => {
      await openLayer();
      view.rerender(<LayerRegion opener={false} />);
      await jump(1);
      const region = button("Details").parentElement;
      if (region === null) throw new Error("the region is not drawn");
      // A mouse press on a blank part of the page, outside the layer, blurs the layer's Close to
      // the body as its button goes down, and its pointer comes up on another element with no
      // click after it, as a browser may send none for a press released off where it began.
      // user-event moves the focus as its pointer goes down, and sends a click on release.
      fireEvent.pointerDown(document.body);
      act(() => button("Close").blur());
      fireEvent.pointerUp(region);
      expect(document.body).toHaveFocus();
      view.rerender(<LayerRegion layer="removed" opener={false} />);
      await jump(1);
      expect(document.body).toHaveFocus();
    });
  } finally {
    view.unmount();
  }
});

test("a layer that closes after the focus came back into the region moves no focus, though its opener is gone", async () => {
  const view = render(<LayerRegion />);
  try {
    await onFakeClock(async () => {
      await openLayer();
      // The reader comes back into the region while the layer is open, and a `blur()` then puts
      // the focus on the body, which no press did and which loses nothing of the region's.
      act(() => button("Second fallback").focus());
      act(() => button("Second fallback").blur());
      await act(async () => {});
      view.rerender(<LayerRegion opener={false} />);
      await jump(1);
      view.rerender(<LayerRegion layer="removed" opener={false} />);
      await jump(1);
      expect(document.body).toHaveFocus();
    });
  } finally {
    view.unmount();
  }
});

test("a layer that closes after the reader went on to the page moves no focus, though its opener is gone", async () => {
  const view = render(<LayerRegion loadMore />);
  try {
    await onFakeClock(async () => {
      await openLayer();
      view.rerender(<LayerRegion loadMore opener={false} />);
      await jump(1);
      // The reader goes on to the page's Load more, outside the layer and the region, and the
      // button then goes, as one does once nothing is left to load: the focus falls to the body.
      act(() => button("Load more").focus());
      view.rerender(<LayerRegion opener={false} />);
      expect(document.body).toHaveFocus();
      await jump(1);
      // Details hides with no press of the reader's.
      view.rerender(<LayerRegion layer="hidden" opener={false} />);
      await jump(1);
      expect(document.body).toHaveFocus();
    });
  } finally {
    view.unmount();
  }
});

test("a layer that closes after a dialog the app drew gave the focus back into it hands the focus to the first fallback", async () => {
  const view = render(<LayerRegion />);
  try {
    await onFakeClock(async () => {
      await openLayer();
      view.rerender(<LayerRegion opener={false} />);
      await jump(1);
      // The search palette opens in the app's root, beside the layer and the region, and takes
      // the focus: it is another layer, not the page.
      view.rerender(<LayerRegion opener={false} palette />);
      act(() => button("Search").focus());
      // Escape shuts the palette, which gives the focus back to the layer's Close, which held it.
      view.rerender(<LayerRegion opener={false} />);
      act(() => button("Close").focus());
      await jump(1);
      expect(button("Close")).toHaveFocus();
      // Details closes by hiding its aside, with the focus still on its Close.
      view.rerender(<LayerRegion layer="hidden" opener={false} />);
      await jump(1);
      expect(button("First fallback")).toHaveFocus();
    });
  } finally {
    view.unmount();
  }
});

/**
 * A region whose Opener stands in a box beside the box's Chip, alone or inside a wrapper of its
 * own that goes with it, and beside the region a layer with a Close. The keeper's first fallback
 * finds the Chip from where the Opener stood, and adds each place it reads to `places`.
 */
function PlaceLayerRegion({
  layer = "open",
  opener = true,
  places,
  wrapped,
}: {
  layer?: LayerState;
  opener?: boolean;
  places: (HTMLElement | null)[];
  wrapped: boolean;
}) {
  const region = useRef<HTMLDivElement>(null);
  useFocusKeeper(region, [
    (loss) => {
      places.push(loss.place);
      return firstInBox(loss);
    },
    ({ root }) => root.querySelector<HTMLElement>("[data-fallback='first']"),
  ]);
  const control = <button type="button">Opener</button>;
  const drawn = wrapped ? <span>{control}</span> : control;
  return (
    <>
      <div ref={region}>
        <div data-box>
          {opener ? drawn : null}
          <button type="button">Chip</button>
        </div>
        <button data-fallback="first" type="button">
          First fallback
        </button>
      </div>
      {layer === "removed" ? null : (
        <aside hidden={layer === "hidden"} {...{ [DETAILS_ATTRIBUTE]: "" }}>
          <button type="button">Close</button>
        </aside>
      )}
    </>
  );
}

test.each([
  ["alone", false],
  ["inside a wrapper that goes with it", true],
] as const)("a layer that closes after a write removed its opener, %s, hands the focus to the control beside where it stood", async (_where, wrapped) => {
  const places: (HTMLElement | null)[] = [];
  const view = render(<PlaceLayerRegion places={places} wrapped={wrapped} />);
  try {
    await onFakeClock(async () => {
      const box = document.querySelector<HTMLElement>("[data-box]");
      await openLayer();
      view.rerender(<PlaceLayerRegion opener={false} places={places} wrapped={wrapped} />);
      await jump(1);
      view.rerender(
        <PlaceLayerRegion layer="removed" opener={false} places={places} wrapped={wrapped} />
      );
      await jump(1);
      // The place is the box the Opener, or the wrapper it went with, was taken out of.
      expect(places).toHaveLength(1);
      expect(places[0]).toBe(box);
      expect(button("Chip")).toHaveFocus();
    });
  } finally {
    view.unmount();
  }
});
