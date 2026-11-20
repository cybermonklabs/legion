/**
 * For unit tests only: makes the test's document drop the focus as Chromium does when the
 * element that holds it turns disabled (the HTML focus fixup rule,
 * https://html.spec.whatwg.org/multipage/interaction.html#focus-fixup-rule). Chromium moves the
 * focus of a focused control that turns disabled to the body, and enabling the control again does
 * not bring the focus back, so a keyboard reader whose control is disabled while its write or read
 * is out loses their place on the page. happy-dom keeps the focus on a disabled control, so
 * without this a unit case cannot see that happen.
 *
 * A control is disabled by its own `disabled`, or, for a form control, by a `disabled` fieldset
 * around it; happy-dom's `:disabled` reads only the first, so the second is read here. While this
 * is installed, a `disabled` set on the element that holds the focus, or on a fieldset around the
 * form control that holds it, blurs that element, which leaves the focus on the body. The move
 * runs from a MutationObserver, so it has happened once an awaited `act` or `waitFor` after the
 * change returns. Returns what removes it.
 */
export function emulateChromiumFocusFixup(): () => void {
  const observer = new MutationObserver(() => {
    const active = document.activeElement;
    if (!(active instanceof HTMLElement)) return;
    if (active.matches(":disabled")) {
      // happy-dom's `blur` ignores a disabled element, so the attribute is lifted for the blur and
      // put back in the same callback: the element hears the focus leave it, as in Chromium.
      const disabled = active.getAttribute("disabled") ?? "";
      active.removeAttribute("disabled");
      active.blur();
      active.setAttribute("disabled", disabled);
      return;
    }
    // A form control a disabled fieldset holds is not disabled itself in happy-dom's reading, so
    // its own `blur` takes it.
    if (
      active.matches("button, input, select, textarea") &&
      active.closest("fieldset[disabled]") !== null
    ) {
      active.blur();
    }
  });
  observer.observe(document, { attributeFilter: ["disabled"], attributes: true, subtree: true });
  return () => observer.disconnect();
}
