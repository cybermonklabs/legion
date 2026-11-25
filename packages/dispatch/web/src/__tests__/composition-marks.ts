/** The two marks of a key that an input method presses, each by its name for a row's title and
 *  by the fields a `keydown` takes to carry it. Chromium and Firefox mark such a key
 *  `isComposing`. Safari sends the key that ends a composition after its compositionend,
 *  unmarked, with keyCode 229 (WebKit bug 165004). */
export const COMPOSITION_MARKS = [
  ["isComposing", { isComposing: true }],
  ["keyCode 229", { isComposing: false, keyCode: 229 }],
] as const;
