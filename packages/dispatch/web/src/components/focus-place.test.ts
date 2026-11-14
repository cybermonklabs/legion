import { expect, test } from "bun:test";
import { renderHook, waitFor } from "@testing-library/react";

import { useFocusRescue } from "./focus-place";

// The page's focus rescue, read in happy-dom with the page built by hand. happy-dom fires no
// `focusout` when a focused element is taken away, where Chromium fires one while the element is
// still in the page (measured, INFRA-3135 round 7), so `lose` fires it as Chromium does before the
// element goes. A `blur()` fires its own.

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
  return { buttons, main };
}

/** Takes `element` away while it holds the focus, firing the `focusout` Chromium fires. */
function lose(element: HTMLElement, leave: () => void = () => element.remove()): void {
  element.dispatchEvent(new FocusEvent("focusout", { bubbles: true }));
  leave();
}

/** Lets the rescue's check run: it runs a frame and then a task after the loss, so a frame and
 *  two tasks here come after it. The tasks are zero-delay turns of the event loop that order this
 *  after the check, not a wait for time to pass. */
async function settled(): Promise<void> {
  const frame = Promise.withResolvers<void>();
  requestAnimationFrame(() => frame.resolve());
  await frame.promise;
  for (const _ of [0, 1]) {
    const task = Promise.withResolvers<void>();
    setTimeout(task.resolve, 0);
    await task.promise;
  }
}

/** Runs `body` with the rescue mounted, and takes the page and the rescue away after it. */
async function withRescue(body: () => Promise<void>): Promise<void> {
  const rescue = renderHook(() => useFocusRescue());
  try {
    await body();
  } finally {
    rescue.unmount();
    document.getElementById("main-content")?.remove();
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

// (g) A component's own hand-off runs first, in the frame after the loss as `useDialog`'s does,
// moving the focus only while it is still on the page; the rescue then leaves it where it landed.
// Without a hand-off the same loss is rescued.
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
