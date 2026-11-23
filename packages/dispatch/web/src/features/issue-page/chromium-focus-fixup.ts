/**
 * For unit tests only: makes the test's document drop the focus as Chromium does when the
 * element that holds it turns disabled. Chromium moves the focus of a focused button that turns
 * disabled to the body, and enabling the button again does not bring the focus back, so a
 * keyboard reader whose control is disabled while its write or read is out loses their place on
 * the page. happy-dom keeps the focus on a disabled button, so without this a unit case cannot
 * see that happen.
 *
 * While it is installed, a `disabled` set on the element that holds the focus blurs it, which
 * leaves the focus on the body. The move runs from a MutationObserver, so it has happened once
 * an awaited `act` or `waitFor` after the change returns. Returns what removes it.
 */
export function emulateChromiumFocusFixup(): () => void {
  const observer = new MutationObserver(() => {
    const active = document.activeElement;
    if (!(active instanceof HTMLElement) || !active.matches(":disabled")) return;
    // happy-dom's `blur` ignores a disabled element, so the attribute is lifted for the blur and
    // put back in the same callback: the element hears the focus leave it, as in Chromium.
    const disabled = active.getAttribute("disabled") ?? "";
    active.removeAttribute("disabled");
    active.blur();
    active.setAttribute("disabled", disabled);
  });
  observer.observe(document, { attributeFilter: ["disabled"], attributes: true, subtree: true });
  return () => observer.disconnect();
}
