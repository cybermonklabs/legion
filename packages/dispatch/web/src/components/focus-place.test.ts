import { expect, jest, spyOn, test } from "bun:test";
import { renderHook, waitFor } from "@testing-library/react";

import { SEND_DEADLINE_MS } from "../features/writes/keyed-write";
import { useFocusRescue } from "./focus-place";

// The page's focus rescue, read in happy-dom with the page built by hand. A focused element taken
// away fires a `focusout` in Chromium while it is still in the page, and none at all in Firefox
// (measured, INFRA-3135 round 7) nor in happy-dom: `lose` fires it as Chromium does before the
// element goes, and a bare `remove()` is the Firefox case, which the rescue sees by watching the
// page. A `blur()` fires its own.

/** The main content the shell hands the rescue, as `app.tsx` hands it its `mainRef`. */
const mainRef: { current: HTMLElement | null } = { current: null };

/** The page's main content holding one button per label, in the document. */
function mainOf(...labels: string[]): { main: HTMLElement; buttons: HTMLButtonElement[] } {
  const main = document.createElement("main");
  main.id = "main-content";
  const buttons = labels.map((label) => {
    const button = document.createElement("button");
    button.type = "button";
    button.textContent = label;
    main.append(button);
    return button;
  });
  document.body.append(main);
  mainRef.current = main;
  return { buttons, main };
}

/** Takes `element` away while it holds the focus, firing the `focusout` Chromium fires. */
function lose(element: HTMLElement, leave: () => void = () => element.remove()): void {
  element.dispatchEvent(new FocusEvent("focusout", { bubbles: true }));
  leave();
}

/** One animation frame. happy-dom runs it on the real clock, under a fake one too. */
async function frame(): Promise<void> {
  const done = Promise.withResolvers<void>();
  requestAnimationFrame(() => done.resolve());
  await done.promise;
}

/** Lets the rescue's check run: it runs a frame and then a task after the loss, so a frame and
 *  two tasks here come after it. The tasks are zero-delay turns of the event loop that order this
 *  after the check, not a wait for time to pass. */
async function settled(): Promise<void> {
  await frame();
  for (const _ of [0, 1]) {
    const task = Promise.withResolvers<void>();
    setTimeout(task.resolve, 0);
    await task.promise;
  }
}

/** Runs `body` with the rescue mounted, and takes the page and the rescue away after it. */
async function withRescue(body: () => Promise<void>): Promise<void> {
  const rescue = renderHook(() => useFocusRescue(mainRef));
  try {
    await body();
  } finally {
    rescue.unmount();
    mainRef.current?.remove();
    mainRef.current = null;
    for (const dialog of document.querySelectorAll('[role="dialog"]')) dialog.remove();
    window.history.replaceState(null, "", "/");
  }
}

// (a) A control taken away hands the focus to the control now at its place.
test("a control taken away hands the focus to the control now at its place", async () => {
  await withRescue(async () => {
    const [first, second, third] = mainOf("First", "Second", "Third").buttons;
    second?.focus();
    lose(second as HTMLElement);
    expect(document.activeElement).toBe(document.body);
    await waitFor(() => expect(third).toHaveFocus());
    expect(first).not.toHaveFocus();
  });
});

// (b) With no control after it, the focus goes to the one before it.
test("the last control taken away hands the focus to the one before it", async () => {
  await withRescue(async () => {
    const [, second, third] = mainOf("First", "Second", "Third").buttons;
    third?.focus();
    lose(third as HTMLElement);
    await waitFor(() => expect(second).toHaveFocus());
  });
});

// (c) A control its own press disabled gets the focus back once it is enabled again.
test("a control that disabled itself gets the focus back when it is enabled again", async () => {
  await withRescue(async () => {
    const [, saving, after] = mainOf("Before", "Save", "After").buttons;
    const button = saving as HTMLButtonElement;
    button.focus();
    // Chromium fires the `focusout` just after the disable; happy-dom blurs no disabled button,
    // so here it comes just before it. The check reads the page only later either way.
    button.blur();
    button.disabled = true;
    await settled();
    // While the write is out the rescue waits on it, rather than sending the focus elsewhere.
    expect(document.activeElement).toBe(document.body);
    expect(after).not.toHaveFocus();
    button.disabled = false;
    await waitFor(() => expect(button).toHaveFocus());
  });
});

// (h) A component that watches its own disabled control, as the Agents composer watches its
// fieldset (`refocusComposer`), starts its watcher after the rescue's wait has begun, and hands
// the focus to its own target once the control is enabled again: the focus ends on that target.
// The rescue settles only after the components have had their turn with that same change.
test("a component's own watcher, started after the rescue's wait, hands the focus on first", async () => {
  await withRescue(async () => {
    const [box, sending] = mainOf("Message", "Send", "After").buttons;
    const send = sending as HTMLButtonElement;
    send.focus();
    send.blur();
    send.disabled = true;
    await settled();
    expect(document.activeElement).toBe(document.body);
    const watcher = new MutationObserver(() => {
      if (send.matches(":disabled")) return;
      watcher.disconnect();
      if (document.activeElement === document.body) box?.focus();
    });
    watcher.observe(send, { attributeFilter: ["disabled"] });
    send.disabled = false;
    await settled();
    expect(box).toHaveFocus();
    expect(send).not.toHaveFocus();
  });
});

// (k) A control the press disabled and enabled again before the check ran is no control the
// reader moved off: it was taken from them, so it gets the focus back.
test("a control enabled again before the check runs gets the focus back", async () => {
  await withRescue(async () => {
    const [, saving, after] = mainOf("Before", "Save", "After").buttons;
    const button = saving as HTMLButtonElement;
    button.focus();
    button.blur();
    button.disabled = true;
    // The `focusout` Chromium fires once the control is disabled, which happy-dom does not.
    button.dispatchEvent(new FocusEvent("focusout", { bubbles: true }));
    button.disabled = false;
    await settled();
    expect(button).toHaveFocus();
    expect(after).not.toHaveFocus();
  });
});

// (l) The wait outlasts the app's write deadline, so a slow write's control, enabled again after
// the 10 s the wait used to give it, still gets the focus back rather than losing it elsewhere.
test("a control disabled for longer than 10 s still gets the focus back when it is enabled", async () => {
  jest.useFakeTimers();
  try {
    await withRescue(async () => {
      const [, saving, after] = mainOf("Before", "Save", "After").buttons;
      const button = saving as HTMLButtonElement;
      button.focus();
      button.blur();
      button.disabled = true;
      await frame();
      jest.advanceTimersByTime(0);
      jest.advanceTimersByTime(SEND_DEADLINE_MS - 1_000);
      await frame();
      jest.advanceTimersByTime(0);
      expect(document.activeElement).toBe(document.body);
      expect(after).not.toHaveFocus();
      button.disabled = false;
      await frame();
      await frame();
      jest.advanceTimersByTime(0);
      expect(button).toHaveFocus();
    });
  } finally {
    jest.useRealTimers();
  }
});

// (d) A dialog that closes over an opener its own press disabled hands the focus to the
// opener's place in the main content, the region under the dialog.
test("a control in a dialog that closes hands the focus to the opener's place under it", async () => {
  await withRescue(async () => {
    const [, opener, after] = mainOf("Before", "Decline", "After").buttons;
    const dialog = document.createElement("div");
    dialog.setAttribute("role", "dialog");
    dialog.setAttribute("aria-modal", "true");
    const confirm = document.createElement("button");
    confirm.type = "button";
    confirm.textContent = "Confirm";
    dialog.append(confirm);
    document.body.append(dialog);
    opener?.focus();
    confirm.focus();
    // The confirm disables the opener while its write is out and closes the dialog, whose
    // return of the focus to that opener takes nothing.
    (opener as HTMLButtonElement).disabled = true;
    lose(confirm, () => dialog.remove());
    opener?.focus();
    expect(document.activeElement).toBe(document.body);
    await waitFor(() => expect(after).toHaveFocus());
  });
});

// (m) A panel that is no modal - a menu's, drawn outside the main content - closed by a press on
// the page outside it, hands the focus back to the trigger that opened it, not to the last
// control of the document.
test("a panel that is no modal, closed by a press outside the main content, hands the focus to its trigger", async () => {
  await withRescue(async () => {
    const [trigger] = mainOf("Filters", "Other", "Last").buttons;
    const panel = document.createElement("div");
    panel.setAttribute("role", "dialog");
    const box = document.createElement("input");
    panel.append(box);
    document.body.append(panel);
    trigger?.focus();
    box.focus();
    box.blur();
    panel.remove();
    await waitFor(() => expect(trigger).toHaveFocus());
  });
});

// (r) WebKit can hold a page's animation frames back for seconds while nothing is drawn: the
// check goes on without its frame rather than leaving the reader on the page that long.
test("a loss is answered even while the page's animation frames are held back", async () => {
  const frames = spyOn(window, "requestAnimationFrame").mockImplementation(() => 0);
  try {
    await withRescue(async () => {
      const [, second, third] = mainOf("First", "Second", "Third").buttons;
      second?.focus();
      lose(second as HTMLElement);
      await waitFor(() => expect(third).toHaveFocus());
    });
  } finally {
    frames.mockRestore();
  }
});

// (q) A control the reader's own Escape takes away - an editor Escape closes - is the reader
// leaving it: nothing moves, so a heading that opens the editor when it takes the focus is not
// handed the focus and the editor stays closed.
test("a control taken away by the reader's Escape moves nothing", async () => {
  await withRescue(async () => {
    const [, editor, heading] = mainOf("Before", "Editor", "Heading").buttons;
    editor?.focus();
    document.dispatchEvent(new KeyboardEvent("keydown", { bubbles: true, key: "Escape" }));
    lose(editor as HTMLElement);
    await settled();
    expect(document.activeElement).toBe(document.body);
    expect(heading).not.toHaveFocus();
  });
});

// (p) A press on the page while the rescue waits on a disabled control is the reader moving on:
// the control enabled again later does not pull the focus back from the page.
test("a pointer press on the page during the wait leaves the focus there when the control is enabled", async () => {
  await withRescue(async () => {
    const [, saving] = mainOf("Message", "Send").buttons;
    const send = saving as HTMLButtonElement;
    send.focus();
    send.blur();
    send.disabled = true;
    await settled();
    document.body.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true }));
    send.disabled = false;
    await settled();
    expect(document.activeElement).toBe(document.body);
  });
});

// (o) A dialog the reader opened with the focus on the page itself - a single-key shortcut with
// nothing focused - gives the focus back to the page when it closes. That is where the reader
// was, so it is no loss: nothing moves, the page's search box least of all, which would take the
// reader's next key as text.
test("a modal dialog opened with the focus on the page closes and leaves the focus on the page", async () => {
  await withRescue(async () => {
    const { main } = mainOf("First", "Second");
    const search = document.createElement("input");
    search.setAttribute("data-control", "search-box");
    main.prepend(search);
    const dialog = document.createElement("div");
    dialog.setAttribute("role", "dialog");
    dialog.setAttribute("aria-modal", "true");
    const box = document.createElement("input");
    dialog.append(box);
    document.body.append(dialog);
    expect(document.activeElement).toBe(document.body);
    box.focus();
    lose(box, () => dialog.remove());
    await settled();
    expect(document.activeElement).toBe(document.body);
    expect(search).not.toHaveFocus();
  });
});

// (i) Firefox and WebKit fire no `focusout` for a focused control taken away: the rescue sees it
// go by watching the page, and hands the focus on all the same.
test("a control taken away with no focusout, as Firefox does, hands the focus to the control now at its place", async () => {
  await withRescue(async () => {
    const [, second, third] = mainOf("First", "Second", "Third").buttons;
    second?.focus();
    second?.remove();
    await waitFor(() => expect(third).toHaveFocus());
  });
});

// (j) So for a control whose parent is taken away: its place is where the parent stood.
test("a control whose parent is taken away with no focusout hands the focus to the control after the parent", async () => {
  await withRescue(async () => {
    const { buttons, main } = mainOf("First", "Last");
    const [first, last] = buttons;
    const group = document.createElement("div");
    const inner = ["Inner one", "Inner two"].map((label) => {
      const button = document.createElement("button");
      button.type = "button";
      button.textContent = label;
      group.append(button);
      return button;
    });
    main.insertBefore(group, last ?? null);
    inner[1]?.focus();
    group.remove();
    await waitFor(() => expect(last).toHaveFocus());
    expect(first).not.toHaveFocus();
  });
});

// (e) A focus the reader put on the page stays there; the same control taken away would have
// been rescued.
test("a deliberate blur onto the page moves nothing", async () => {
  await withRescue(async () => {
    const [first, second] = mainOf("First", "Second").buttons;
    first?.focus();
    first?.blur();
    await settled();
    expect(document.activeElement).toBe(document.body);

    second?.focus();
    lose(second as HTMLElement);
    await waitFor(() => expect(first).toHaveFocus());
  });
});

// (f) A loss on a page the reader has since left moves nothing: the new page owns its focus. The
// same loss on the page it happened on is rescued.
test("a loss followed by a change of page moves nothing", async () => {
  await withRescue(async () => {
    const [first, second, third] = mainOf("First", "Second", "Third").buttons;
    second?.focus();
    lose(second as HTMLElement);
    window.history.pushState(null, "", "/elsewhere");
    await settled();
    expect(document.activeElement).toBe(document.body);

    first?.focus();
    lose(first as HTMLElement);
    await waitFor(() => expect(third).toHaveFocus());
  });
});

// (n) A navigation writes the new address before the old page goes, so the loss is read against
// the address the control was focused on: the change comes first here, then the control goes.
test("a control taken away after the address changed moves nothing", async () => {
  await withRescue(async () => {
    const [, second, third] = mainOf("First", "Second", "Third").buttons;
    second?.focus();
    window.history.pushState(null, "", "/elsewhere");
    lose(second as HTMLElement);
    await settled();
    expect(document.activeElement).toBe(document.body);
    expect(third).not.toHaveFocus();
  });
});

// (g) A component's own hand-off runs first - here in the frame after the loss, moving the focus
// only while it is still on the page - and the rescue then leaves it where it landed. Without a
// hand-off the same loss is rescued.
test("a component that already moved the focus is left alone", async () => {
  await withRescue(async () => {
    const [first, second, third] = mainOf("First", "Second", "Third").buttons;
    first?.focus();
    lose(first as HTMLElement);
    requestAnimationFrame(() => {
      if (document.activeElement === document.body) third?.focus();
    });
    await settled();
    expect(third).toHaveFocus();
    expect(second).not.toHaveFocus();

    lose(third as HTMLElement);
    await waitFor(() => expect(second).toHaveFocus());
  });
});
