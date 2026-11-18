import { expect, test } from "bun:test";
// biome-ignore lint/style/noRestrictedImports: the press case needs a press, a blur and a removal inside one task, and user-event's press awaits between its events.
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useEffect, useRef, useState } from "react";

import { jump, onFakeClock } from "../../__tests__/fake-clock";
import { userOnFakeClock } from "../../__tests__/user";
import { emulateChromiumFocusFixup } from "./chromium-focus-fixup";
import { useFocusKeeper } from "./focus-keeper";
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

/** A box whose Act control is drawn inside a wrapper of its own, which goes with it, beside the
 *  box's Chip. The keeper's one fallback finds the box from where Act stood. */
function BoxRegion({ shown = true }: { shown?: boolean }) {
  const region = useRef<HTMLDivElement>(null);
  useFocusKeeper(region, [({ place }) => place?.closest("[data-box]")?.querySelector("button")]);
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
 * A region holding an Opener and the two fallback buttons, and beside it, outside the region, a
 * layer: Details' `aside`, holding a Close as Details holds its own, and inside it a dialog of
 * its own drawn as an `aside`, holding a Field. The props remove the Opener, as a write does
 * while the layer is open; remove the Close, the layer's own control; and close the layer.
 */
function LayerRegion({
  close = true,
  layer = "open",
  opener = true,
}: {
  close?: boolean;
  layer?: LayerState;
  opener?: boolean;
}) {
  const region = useRef<HTMLDivElement>(null);
  useFocusKeeper(region, [
    ({ root }) => root.querySelector<HTMLElement>("[data-fallback='first']"),
    ({ root }) => root.querySelector<HTMLElement>("[data-fallback='second']"),
  ]);
  return (
    <>
      <div ref={region}>
        {opener ? <button type="button">Opener</button> : null}
        <button data-fallback="first" type="button">
          First fallback
        </button>
        <button data-fallback="second" type="button">
          Second fallback
        </button>
      </div>
      {layer === "removed" ? null : (
        <aside hidden={layer === "hidden"} {...{ [DETAILS_ATTRIBUTE]: "" }}>
          {close ? <button type="button">Close</button> : null}
          <aside role="dialog">
            <button type="button">Field</button>
          </aside>
        </aside>
      )}
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
