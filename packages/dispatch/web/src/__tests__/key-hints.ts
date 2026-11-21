/** The key a control draws beside its label for the eye (`ControlKeyHint`), inside it and hidden
 *  from assistive tech: each of its `kbd`s' `data-key`, which its CSS draws, so no glyph of it is
 *  text of the page; undefined where the control draws no key. */
export function keyHintOf(control: Element): string | undefined {
  const keys = Array.from(control.querySelectorAll('[aria-hidden="true"] kbd'), (kbd) =>
    kbd.getAttribute("data-key")
  );
  return keys.length === 0 ? undefined : keys.join(" ");
}
