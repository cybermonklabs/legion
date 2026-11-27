/** The HTML focus fixup rule, which a browser runs and happy-dom does not: a focused control that
 *  becomes disabled loses the focus to the document's body
 *  (https://html.spec.whatwg.org/multipage/interaction.html#focus-fixup-rule). A form control is
 *  disabled by its own `disabled`, or by a `disabled` fieldset around it; happy-dom's `:disabled`
 *  reads only the first, so the second is read here. A test of a control that keeps a keyboard
 *  reader's focus through a state it cannot be pressed in runs this, so a `disabled` that would
 *  drop the focus in a browser drops it here too. The body is focused rather than the control
 *  blurred, because happy-dom's `blur()` refuses a disabled element. Returns what stops it. */
export function runFocusFixup(): () => void {
  const observer = new MutationObserver(() => {
    const active = document.activeElement;
    if (
      active instanceof HTMLElement &&
      active.matches("button, input, select, textarea") &&
      (active.matches(":disabled") || active.closest("fieldset[disabled]") !== null)
    ) {
      document.body.focus();
    }
  });
  observer.observe(document.body, {
    attributeFilter: ["disabled"],
    attributes: true,
    subtree: true,
  });
  return () => observer.disconnect();
}
