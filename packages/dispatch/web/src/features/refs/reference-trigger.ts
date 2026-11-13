import { buildDispatchReference, type DispatchReferenceRoute } from "./routes";

/**
 * The attribute a React-rendered link spreads to become a hover-card trigger; the Markdown and
 * Proof surfaces carry the same attributes on the anchors they render themselves. The card that
 * reads it is `RefPreview.tsx`'s, drawn by the shell's reference-preview layer
 * (`RefPreviewLayer.tsx`).
 *
 * In a module of its own, away from the card, so that a link can name its target without
 * pulling the card in. While it was `RefPreview.tsx`'s, every page that drew a reference - the
 * issue header's parent pill and its "referenced by" list most of all - put the whole card in
 * its own chunk's imports: on a cold `/issues/CORE-1`, 10,009 bytes of card and 2,585 of the
 * Markdown preview and the card-bounds module it imports came down before the Spec's first block
 * drew. Measured with the R13 reading of dispatch://INFRA-1109 (Resource Timing of `/assets/*.js`
 * against the first block's insertion), after that issue had moved the issue page's tab panels
 * into chunks of their own.
 */
export function referenceTriggerProps(route: DispatchReferenceRoute): {
  "data-dispatch-ref": string;
} {
  return { "data-dispatch-ref": buildDispatchReference(route) };
}
