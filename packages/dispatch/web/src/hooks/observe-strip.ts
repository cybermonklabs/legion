/**
 * Calls `changed` whenever `strip` or one of its children changes size, and whenever the mutations
 * `watch` names happen in the strip, after observing again the children such a mutation added. A
 * strip that scrolls sideways is redrawn by all three: the viewport resizing it, a child growing (a
 * live count, a title arriving), and a child added or taken away. Answers the function that stops
 * both observers. `components/useScrollEnds.ts` and the tablist of `components/Tabs.tsx` read it.
 * happy-dom has no `ResizeObserver` of its own: the unit suite installs one, and without it a
 * strip is still measured whenever its content changes.
 */
export function observeStrip(
  strip: HTMLElement,
  changed: () => void,
  watch: MutationObserverInit
): () => void {
  const sizes = typeof ResizeObserver === "undefined" ? undefined : new ResizeObserver(changed);
  const observeAll = () => {
    sizes?.observe(strip);
    for (const child of strip.children) {
      sizes?.observe(child);
    }
  };
  observeAll();
  const contents = new MutationObserver(() => {
    observeAll();
    changed();
  });
  contents.observe(strip, watch);
  return () => {
    sizes?.disconnect();
    contents.disconnect();
  };
}
